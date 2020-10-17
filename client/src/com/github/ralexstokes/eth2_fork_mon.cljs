(ns ^:figwheel-hooks com.github.ralexstokes.eth2-fork-mon
  (:require-macros [cljs.core.async.macros :refer [go]])
  (:require
   [cljs.pprint :as pprint]
   [reagent.core :as r]
   [reagent.dom :as r.dom]
   [cljs-http.client :as http]
   [cljs.core.async :refer [<!]]))

(def polling-frequency 1000) ;; 1 second

(defonce state (r/atom {:eth2-spec {}
                        :heads []}))

(defn render-edn [data]
  [:pre
   (with-out-str
     (pprint/pprint data))])

(defn clock-view []
  [:div.row
   (render-edn (:eth2-spec @state))])

(defn compare-heads-view []
  [:div.row
   (render-edn (:heads @state))])

(defn debug-view []
  [:div.row.debug
   (render-edn @state)])

(defn container-row
  "a 'widget" [component]
  [:div.row
   [:div.col]
   [:div.col.align-self-center
    component]
   [:div.col]])

(defn app []
  [:div.container-fluid
   (container-row
    (clock-view))
   (container-row
    (compare-heads-view))
   (container-row
    (debug-view))])

(defn mount []
  (r.dom/render [app] (js/document.getElementById "root")))

(defn ^:after-load re-render [] (mount))

(defn load-spec-from-server []
  (go (let [response (<! (http/get "http://localhost:8080/spec"
                                   {:with-credentials? false}))]
        (swap! state assoc :eth2-spec (:body response)))))

(defn fetch-heads []
  (go (let [response (<! (http/get "http://localhost:8080/heads"
                                   {:with-credentials? false}))]
        (swap! state assoc :heads (:body response)))))

(defn start-polling-for-heads []
  (fetch-heads)
  (let [polling-task (js/setInterval fetch-heads polling-frequency)]
    (swap! state assoc :polling-task polling-task)))

(defonce init (do
                (load-spec-from-server)
                (start-polling-for-heads)
                (mount)))
