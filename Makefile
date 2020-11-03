deploy:
	rm -rf public
	cp -r client/resources/public .
	cp -r client/target/public/* ./public
