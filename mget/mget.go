package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/pat"
	logging "github.com/op/go-logging"
	"github.com/vaughan0/go-ini"
	"io"
	"io/ioutil"
	_ "math/big"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	INPUTS []string
	STYPE  string
)

var (
	cfg        ini.File
	CONFIGFILE string
	WORKERNUM  = 10
	WEBENBLED  = false
	HTTPHOST   = "localhost"
	HTTPPORT   = "8080"
)

var (
	LOGFILE   *os.File
	LOGFMT                  = "%{color}%{time:15:04:05.000000} %{shortfunc} ▶ %{level:.4s} %{id:03x}%{color:reset} %{message}"
	LOGFORMAT               = logging.MustStringFormatter(LOGFMT)
	LOG                     = logging.MustGetLogger("logfile")
	GLOGLEVEL logging.Level = logging.DEBUG
)

type FetchedResult struct {
	input   string
	content string
}

type FetchedInput struct {
	m map[string]error
	sync.Mutex
}

type commander interface {
	command()
}

type callFetch struct {
	fetchedInput *FetchedInput
	p            *Pipeline
	result       chan FetchedResult
	input        string
}

func fetchHtml(input string) (string, error) {
	if input == "" {
		return "", nil
	}

	LOG.Debug("= input: ", input)
	res, err := http.Get(input)
	if err != nil {
		LOG.Panic(err)
		return "", err
	}
	defer res.Body.Close()
	doc, err := ioutil.ReadAll(res.Body)
	if err != nil {
		LOG.Panic(err)
		return "", err
	} else {
		LOG.Debug(string(doc))
	}
	return string(doc), nil
}

func fetchCmd(input string) (string, error) {
	if input == "" {
		return "", nil
	}

	LOG.Debug("==============================================================")
	LOG.Debug("= input: ", input)
	doc, err := exeCmd(input)
	if err != nil {
		LOG.Panic(err)
		return "", err
	} else {
		LOG.Debug(doc)
		fmt.Printf("%s", doc)
	}
	return string(doc), nil
}

func exeCmd(cmd string) (string, error) {
	parts := strings.Fields(cmd)
	head := parts[0]
	parts = parts[1:len(parts)]
	out, err := exec.Command(head, parts...).Output()
	if err != nil {
		return "", err
	}
	return string(out), err
}

func (g *callFetch) request(input string) {
	g.p.request <- &callFetch{
		fetchedInput: g.fetchedInput,
		p:            g.p,
		result:       g.result,
		input:        input,
	}
}

func (g *callFetch) parseContent(input string, doc string) <-chan string {
	content := make(chan string)
	go func() {
		content <- doc
		chk := false
		val := ""
		g.fetchedInput.Lock()
		for n := range INPUTS {
			if _, ok := g.fetchedInput.m[INPUTS[n]]; !ok {
				chk = true
				val = INPUTS[n]
				g.request(val)
				break
			}
		}
		if chk == false {
		}
		g.fetchedInput.Unlock()
	}()
	return content
}

func (g *callFetch) command() {
	g.fetchedInput.Lock()
	if _, ok := g.fetchedInput.m[g.input]; ok {
		g.fetchedInput.Unlock()
		return
	}
	g.fetchedInput.Unlock()

	var doc string
	var err error
	if g.input != "" {
		if STYPE == "cmd" {
			doc, err = fetchCmd(g.input)
			if err != nil {
				go func(u string) {
					g.request(u)
				}(g.input)
				return
			}
		} else {
			doc, err = fetchHtml(g.input)
			if err != nil {
				go func(u string) {
					g.request(u)
				}(g.input)
				return
			}
		}
	}

	g.fetchedInput.Lock()
	g.fetchedInput.m[g.input] = err
	g.fetchedInput.Unlock()

	content := <-g.parseContent(g.input, doc)
	g.result <- FetchedResult{g.input, content}
}

type Pipeline struct {
	request chan commander
	done    chan struct{}
	wg      *sync.WaitGroup
}

func NewPipeline() *Pipeline {
	return &Pipeline{
		request: make(chan commander),
		done:    make(chan struct{}),
		wg:      new(sync.WaitGroup),
	}
}

func (p *Pipeline) Worker() {
	for r := range p.request {
		select {
		case <-p.done:
			return
		default:
			r.command()
		}
	}
}

func (p *Pipeline) Run() {
	p.wg.Add(WORKERNUM)
	for i := 0; i < WORKERNUM; i++ {
		go func() {
			p.Worker()
			p.wg.Done()
		}()
	}

	go func() {
		p.wg.Wait()
	}()
}

func mainExec() map[string]string {
	start := time.Now()
	numCPUs := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPUs)

	p := NewPipeline()
	p.Run()

	call := &callFetch{
		fetchedInput: &FetchedInput{m: make(map[string]error)},
		p:            p,
		result:       make(chan FetchedResult),
		input:        "",
	}
	p.request <- call

	result := make(map[string]string)
	count := 0
	LOG.Debug("len(INPUTS): ", len(INPUTS))
	for a := range call.result {
		count++
		countStr := strconv.Itoa(count)
		result[countStr] = a.content
		LOG.Debug("count: ", count)
		if count > len(INPUTS) {
			close(p.done)
			break
		}
	}

	elapsed := time.Since(start)
	LOG.Debug("elapsed: ", elapsed)
	return result
}

// http://localhost:8080/mcall/cmd/{"inputs":[{"input":"ls -al"},{"input":"ls"}]}
func getHandle(w http.ResponseWriter, r *http.Request) {
	STYPE = r.URL.Query().Get(":type")
	paramStr := r.URL.Query().Get(":params")
	LOG.Debug(STYPE, paramStr)

	getInput(paramStr)
	b := makeResponse()
	w.Write(b)
}

// http://localhost:8080/mcall?type=post&params={"inputs":[{"input":"http://core.local.xdn.com/test1","id":"aaa","pswd":"bbb"},{"input":"http://core.local.xdn.com/test2","id":"aaa","pswd":"bbb"}]}
func postHandle(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		LOG.Debugf("ParseForm %s", err)
	}
	LOG.Debugf("\n what we got was %+v\n", r.Form)

	if STYPE = r.FormValue("type"); STYPE == "" {
		LOG.Warning(fmt.Sprintf("bad STYPE received %+v", r.Form["type"]))
		return
	}

	var paramStr = ""
	if paramStr = r.FormValue("params"); paramStr == "" {
		LOG.Warning(fmt.Sprintf("bad params received %+v", r.Form["params"]))
		return
	}
	LOG.Debug(STYPE, paramStr)

	getInput(paramStr)
	b := makeResponse()
	io.WriteString(w, string(b))
}

func makeResponse() []byte {
	result := mainExec()

	res := make(map[string]string)
	res["status"] = "OK"
	res["ts"] = time.Now().String()
	str, err := json.Marshal(result)
	res["count"] = strconv.Itoa(len(result) - 1)
	res["result"] = string(str)

	b, err := json.Marshal(res)
	if err != nil {
		LOG.Errorf("error: %s", err)
	}
	return b
}

func webserver() {
	killch := make(chan os.Signal, 1)
	signal.Notify(killch, os.Interrupt)
	signal.Notify(killch, syscall.SIGTERM)
	signal.Notify(killch, syscall.SIGINT)
	signal.Notify(killch, syscall.SIGQUIT)
	go func() {
		<-killch
		LOG.Fatalf("Interrupt %s", time.Now().String())
	}()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		r := pat.New()
		r.Get("/mcall/{type}/{params}", http.HandlerFunc(getHandle))
		r.Post("/mcall", http.HandlerFunc(postHandle))
		http.Handle("/", r)
		LOG.Debug("Listening: ", HTTPHOST, HTTPPORT)
		err := http.ListenAndServe(HTTPHOST+":"+HTTPPORT, nil)
		if err != nil {
			LOG.Fatalf("ListenAndServe: ", err)
		}
		wg.Done()
	}()

	wg.Wait()
}

func getInput(aInput string) {
	type Inputs struct {
		Inputs []map[string]interface{} `json:"inputs"`
	}
	var data Inputs
	err := json.Unmarshal([]byte(aInput), &data)
	if err != nil {
		LOG.Panic("Unmarshal error %s", err)
	}
	for i := range data.Inputs {
		input := data.Inputs[i]["input"]
		INPUTS = append(INPUTS, input.(string))
	}
}

//////////////////////////////////////////////////////////////////////////////////////////////////////
// 2 ways of run
// - 1st: mget web
// 		call from brower: http://localhost:8080/main/core/1418,1419,2502,2694,2932,2933,2695
// - 2nd: mget core/graphite 1418,1419,2502,2694,2932,2933,2695
//////////////////////////////////////////////////////////////////////////////////////////////////////
func main() {

	if len(os.Args) < 2 {
		fmt.Println("No parameter!")
		return
	} else {
		// mcall --t=get --i=http://core.local.xdn.com/1/stats/uptime_list?company_id=1^start_time=1464636372^end_time=1464722772^hc_id=1418
		// mcall --c=/Users/dhong/Documents/workspace/go/src/tz.com/tz_mcall/etc/mcall.cfg
		allArgs := os.Args[1:]
		////[ argument ]////////////////////////////////////////////////////////////////////////////////
		for i := range allArgs {
			str := allArgs[i]
			key := str[0:strings.Index(str, "=")]
			val := str[strings.Index(str, "=")+1 : len(str)]
			switch key {
			case "--t":
				STYPE = val
			case "--i":
				INPUTS = append(INPUTS, val)
			case "--c":
				CONFIGFILE = val
			case "--w":
				if val == "on" {
					WEBENBLED = true
				}
			}
		}
		if STYPE == "" {
			STYPE = "cmd"
		}

		logfile := "/var/log/mcall/mcall.log"
		loglevel := "DEBUG"
		////[ configuratin file ]////////////////////////////////////////////////////////////////////////////////
		if CONFIGFILE != "" {
			cfg, err := ini.LoadFile(CONFIGFILE)
			if err != nil {
				fmt.Println("parse config "+CONFIGFILE+" file error: ", err)
			}

			loglevel, _ = cfg.Get("log", "level")
			logfile, _ = cfg.Get("log", "file")

			workerNumber, ok := cfg.Get("worker", "number")
			if !ok {
				fmt.Println("'file' missing from 'worker", "number")
			} else {
				WORKERNUM, _ = strconv.Atoi(workerNumber)
			}

			webEnbleStr, ok := cfg.Get("webserver", "enable")
			if !ok {
				fmt.Println("'enable' missing from 'webserver", "enable")
			} else {
				if webEnbleStr == "on" {
					WEBENBLED = true
				} else {
					WEBENBLED = false
				}
			}

			if WEBENBLED == true {
				httpost, ok := cfg.Get("webserver", "host")
				if !ok {
					fmt.Println("'host' missing from 'webserver", "host")
				} else {
					HTTPHOST = httpost
				}

				httpport, ok := cfg.Get("webserver", "port")
				if !ok {
					fmt.Println("'port' missing from 'webserver", "port")
				} else {
					HTTPPORT = httpport
				}
			} else {
				input, ok := cfg.Get("request", "input")
				if !ok {
					fmt.Println("'input' missing from 'request' section")
				}
				stype, _ := cfg.Get("request", "type")
				if stype != "" {
					STYPE = stype
				}
				getInput(input)
			}
		}

		////[ log file ]////////////////////////////////////////////////////////////////////////////////
		if _, err := os.Stat(logfile); err != nil {
			logfile, _ := os.Getwd()
			logfile = logfile + "/mget.log"
		}

		LOGFILE, err := os.OpenFile(logfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			LOG.Fatalf("Log file error: %s %s", logfile, err)
		}
		defer func() {
			LOGFILE.WriteString(fmt.Sprintf("closing %s", time.UnixDate))
			LOGFILE.Close()
		}()

		logback := logging.NewLogBackend(LOGFILE, "", 0)
		logformatted := logging.NewBackendFormatter(logback, LOGFORMAT)
		GLOGLEVEL, err := logging.LogLevel(loglevel)
		if err != nil {
			GLOGLEVEL = logging.DEBUG
		}
		logging.SetBackend(logformatted)
		logging.SetLevel(GLOGLEVEL, "")

		LOG.Debug("workerNumber: ", WORKERNUM)
		LOG.Debug("type: ", STYPE)
		LOG.Debug("webEnabled: ", WEBENBLED)
		LOG.Debug("httphost: ", HTTPHOST)
		LOG.Debug("httpport: ", HTTPPORT)

		////[ run app ]////////////////////////////////////////////////////////////////////////////////
		if WEBENBLED == true {
			webserver()
		} else {
			mainExec()
		}
	}
}
