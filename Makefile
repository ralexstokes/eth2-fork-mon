build-prod:
	cd client
	clojure -m figwheel.main -O simple -bo prod
	cd -
copy-prod:
	mkdir -p public/cljs-out
	cp -r client/target/public/cljs-out/prod* public/cljs-out
deploy-docker-prod:
	docker build -t ralexstokes/eth2-fork-mon .
	docker push ralexstokes/eth2-fork-mon
