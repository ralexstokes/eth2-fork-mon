(ns ^:figwheel-hooks com.github.ralexstokes.eth2-fork-mon
  (:require
   [reagent.core :as r]
   [reagent.dom :as r.dom]))

(defonce state (r/atom {}))

(defn app []
  [:div
   [:some-data
    "hi"]])

(defn mount []
  (r.dom/render [app] (js/document.getElementById "root")))

(defn ^:after-load re-render [] (mount))

(defonce init (mount))
