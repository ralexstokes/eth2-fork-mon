build-prod:
	cd client
	clojure -m figwheel.main -O simple -bo prod
	cd -
copy-prod:
	mkdir -p public/cljs-out
	cp -r client/target/public/cljs-out/prod* public/cljs-out
