cd ..

chmod 775 deb/pkg-build/DEBIAN/postinst

cd deb

dpkg -b pkg-build

mv pkg-build.deb tz_mcall.deb
