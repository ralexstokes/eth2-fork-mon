(ns ^:figwheel-hooks com.github.ralexstokes.eth2-fork-mon
  (:require-macros [cljs.core.async.macros :refer [go]])
  (:require
   [clojure.string :as str]
   [cljs.pprint :as pprint]
   [reagent.core :as r]
   [reagent.dom :as r.dom]
   [cljs-http.client :as http]
   [cljs.core.async :refer [<!]]))

(def debug-mode? false)

(defn- get-time []
  (.now js/Date))

(defn- in-seconds [time]
  (.floor js/Math
          (/ time 1000)))

(defn slot-from-timestamp [ts genesis-time seconds-per-slot]
  (quot (- ts genesis-time)
        seconds-per-slot))

(defn- calculate-eth2-time [genesis-time seconds-per-slot slots-per-epoch]
  (let [current-time (get-time)
        time-in-secs (in-seconds current-time)
        slot (slot-from-timestamp time-in-secs genesis-time seconds-per-slot)
        slot-start-in-seconds  (+ genesis-time (* slot seconds-per-slot))]
    {:slot slot
     :epoch (quot slot slots-per-epoch)
     :slot-in-epoch (mod slot slots-per-epoch)
     :progress-into-slot (* 100 (/ (- current-time (* slot-start-in-seconds 1000)) (* seconds-per-slot 1000)))
     :seconds-into-slot (- time-in-secs slot-start-in-seconds)}))

(defonce state (r/atom {}))

;; debug utility
(defn render-edn [data]
  [:pre
   (with-out-str
     (pprint/pprint data))])

(defn clock-view []
  (when-let [eth2-spec (:eth2-spec @state)]
    (let [{:keys [seconds_per_slot slots_per_epoch]} eth2-spec
          seconds-per-slot seconds_per_slot
          slots-per-epoch slots_per_epoch
          {:keys [slot epoch slot-in-epoch seconds-into-slot progress-into-slot]} (:slot-clock @state)]
      [:div.card
       [:div.card-header
        "Clock"]
       [:div.card-body
        [:p (str "Epoch: " epoch " (slot: " slot ")")]
        [:p (str "Slot in epoch: " slot-in-epoch " / " slots-per-epoch)]
        "Progress through slot:"
        [:div.progress
         [:div.progress-bar
          {:style
           {:width (str progress-into-slot "%")}}]]]])))

(defn peer-view [index {:keys [name version]}]
  [:tr {:key index}
   [:th {:scope :row}
    name]
   [:td version]])

(defn nodes-view []
  (when-let [peers (:heads @state)]
    [:div#nodes-drawer.accordion
     [:div.card
      [:div.card-header
       [:button.btn.btn-link.btn-block.text-left {:type :button
                                                  :data-toggle "collapse"
                                                  :data-target "#collapseNodes"}
        "Nodes"]]
      [:div#collapseNodes.collapse.show {:data-parent "#nodes-drawer"}
       [:div.card-body
        [:table.table.table-hover
         [:thead
          [:tr
           [:th {:scope :col} "Name"]
           [:th {:scope :col} "Version"]]]
         [:tbody
          (map-indexed peer-view peers)]]]]]]))

(defn humanize-hex [hex-str]
  (str (subs hex-str 0 6)
       "..."
       (subs hex-str (- (count hex-str) 4))))

(defn head-view [index {:keys [name slot root is-majority?]}]
  [:tr {:class (if is-majority? :table-success :table-danger)
        :key index}
   [:th {:scope :row}
    name]
   [:td [:a {:href (str "https://beaconcha.in/block/" slot)} slot]]
   [:td [:a {:href (str "https://beaconcha.in/block/" (subs root 2))} (humanize-hex root)]]])

(defn compare-heads-view []
  (when-let [heads (:heads @state)]
    [:div.card
     [:div.card-header
      "Latest head by node"]
     [:div.card-body
      [:table.table.table-hover
       [:thead
        [:tr
         [:th {:scope :col} "Name"]
         [:th {:scope :col} "Slot"]
         [:th {:scope :col} "Root"]]]
       [:tbody
        (map-indexed head-view heads)]]]]))

(defn tree-view []
  [:div.card
   [:div.card-header
    "Block tree"]
   [:div.card-body
    [:div#block-tree-viewer
     [:p
      "Count of heads: " (get @state :head-count 0)]]]])

(defn debug-view []
  [:div.row.debug
   (render-edn @state)])

(defn container-row
  "layout for a 'widget'"
  [component]
  [:div.row.my-2
   [:div.col]
   [:div.col-10
    component]
   [:div.col]])

(defn app []
  [:div.container-fluid
   [:navbar.navbar.navbar-expand-sm.navbar-light.bg-light
    [:div.navbar-brand "eth2 fork mon"]
    [:nav
     [:div.nav.nav-tabs
      [:a.nav-link.active {:data-toggle :tab
                           :href "#nav-tip-monitor"} "chain monitor"]
      [:a.nav-link {:data-toggle :tab
                    :href "#nav-heads-monitor"} "block tree"]]]]
   [:div.tab-content
    (container-row
     (clock-view))
    [:div#nav-tip-monitor.tab-pane.fade.show.active
     (container-row
      (nodes-view))
     (container-row
      (compare-heads-view))]
    [:div#nav-heads-monitor.tab-pane.fade.show
     (container-row
      (tree-view))]
    (when debug-mode?
      (container-row
       (debug-view)))]])

(defn mount []
  (r.dom/render [app] (js/document.getElementById "root")))

(defn ^:after-load re-render [] (mount))

(defn load-spec-from-server []
  (go (let [response (<! (http/get "http://localhost:8080/spec"
                                   {:with-credentials? false}))]
        (swap! state assoc :eth2-spec (:body response)))))

(defn process-heads-response [heads]
  (->> heads
       (map :root)
       frequencies
       (sort-by val >)
       first))

(defn attach-majority [majority-root head]
  (assoc head :is-majority? (= (:root head) majority-root)))

(defn- name-from [version]
  (-> version
      (str/split "/")
      first))

(defn attach-name [peer]
  (assoc peer :name (name-from (:version peer))))

(defn fetch-heads []
  (go (let [response (<! (http/get "http://localhost:8080/heads"
                                   {:with-credentials? false}))
            heads (:body response)
            [majority-root _] (process-heads-response heads)]
        (swap! state assoc :heads (->> heads
                                       (map (partial attach-majority majority-root))
                                       (map attach-name))))))

(def polling-frequency 700) ;; ms
(def slot-clock-refresh-frequency 100) ;; ms

(defn start-polling-for-heads []
  (fetch-heads)
  (let [polling-task (js/setInterval fetch-heads polling-frequency)]
    (swap! state assoc :polling-task polling-task)))

(defn fetch-block-tree []
  (go (let [response (<! (http/get "http://localhost:8080/block-tree"
                                   {:with-credentials? false}))
            response-body (:body response)
            head-count (:head_count response-body)]
        (swap! state assoc :head-count head-count))))

(defn start-polling-for-block-tree []
  (fetch-block-tree)
  (let [block-tree-task (js/setInterval fetch-block-tree polling-frequency)]
    (swap! state assoc :block-tree-task block-tree-task)))

(defn update-slot-clock []
  (when-let [eth2-spec (:eth2-spec @state)]
    (let [genesis-time (:genesis_time eth2-spec)
          seconds-per-slot (:seconds_per_slot eth2-spec)
          slots-per-epoch (:slots_per_epoch eth2-spec)]
      (swap! state assoc :slot-clock (calculate-eth2-time genesis-time seconds-per-slot slots-per-epoch)))))

(defn start-slot-clock []
  (let [timer-task (js/setInterval update-slot-clock slot-clock-refresh-frequency)]
    (swap! state assoc :timer-task timer-task)))

(defonce init (do
                (load-spec-from-server)
                (start-slot-clock)
                (start-polling-for-heads)
                (start-polling-for-block-tree)
                (mount)))
