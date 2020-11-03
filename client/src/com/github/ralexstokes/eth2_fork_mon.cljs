(ns ^:figwheel-hooks com.github.ralexstokes.eth2-fork-mon
  (:require-macros [cljs.core.async.macros :refer [go]])
  (:require
   [cljsjs.d3]
   [clojure.string :as str]
   [cljs.pprint :as pprint]
   [reagent.core :as r]
   [reagent.dom :as r.dom]
   [cljs-http.client :as http]
   [cljs.core.async :refer [<! chan close!]]))

(def debug-mode? false)

(def dev-mode? false)

(defn url-for [path]
  (if dev-mode?
    (str "http://localhost:8080/" path)
    (str "/" path)))

(defn put! [& objs]
  (doseq [obj objs]
    (.log js/console obj)))

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
    (let [{:keys [slots_per_epoch]} eth2-spec
          slots-per-epoch slots_per_epoch
          {:keys [slot epoch slot-in-epoch progress-into-slot]} (:slot-clock @state)]
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
  (str (subs hex-str 2 6)
       ".."
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

(defn count-heads [root]
  (.-length (.leaves root)))

(defn tree-view []
  [:div.card
   [:div.card-header
    "Block tree over last 4 epochs"]
   [:div.card-body
    [:div#head-count-viewer
     (when-let [head-count (:head-count @state)]
       [:p
      "Count of heads in beacon node's view: " head-count])
     [:p
      "Canonical head root: " (get @state :majority-root "")]
     [:div#fork-choice.svg-container]]]])

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
   [:div.navbar.navbar-expand-sm.navbar-light.bg-light
    [:div.navbar-brand "eth2 fork mon"]
    [:nav
     [:div.nav.nav-tabs
      [:a.nav-link.active {:data-toggle :tab
                           :href "#nav-tip-monitor"} "chain monitor"]
      [:a.nav-link {:data-toggle :tab
                    :href "#nav-block-tree"} "block tree"]]]]
   [:div.tab-content
    (container-row
     (clock-view))
    [:div#nav-tip-monitor.tab-pane.fade.show.active
     (container-row
      (nodes-view))
     (container-row
      (compare-heads-view))]
    [:div#nav-block-tree.tab-pane.fade.show
     (container-row
      (tree-view))]
    (when debug-mode?
      (container-row
       (debug-view)))]])

(defn mount []
  (r.dom/render [app] (js/document.getElementById "root")))

(defn ^:after-load re-render [] (mount))

(defn fetch-spec-from-server []
  (http/get (url-for "spec") {:with-credentials? false}))

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

(def polling-frequency 700) ;; ms
(def slot-clock-refresh-frequency 100) ;; ms

(defn fetch-head-count []
  (go (let [response (<! (http/get (url-for "block-tree")
                                   {:with-credentials? false}))
            response-body (:body response)
            head-count (:head_count response-body)]
        (swap! state assoc :head-count head-count))))

(defn empty-svg! [svg]
  (.remove svg))

(defn node->label [d]
  (let [data (.-data d)
        root (-> data .-root humanize-hex)]
    (if-let [parent (.-parent d)]
      (if-let [siblings (.-children parent)]
        (if (> (.-length siblings) 1)
            (let [weight (.-weight data)]
              (str/join ", " [root (str (quot weight (js/Math.pow 10 9)) " ETH")]))
          root)
        root)
    root)))

(defn canonical-node? [d]
  (-> d
      .-data
      .-is_canonical))

(defn slot-guide->label [highest-slot offset]
  (let [slot (- highest-slot offset)]
    (if (zero? (mod slot 32))
      (str slot " (epoch " (quot slot 32) ")")
      slot)))

(defn node->y-offset [slot-offset dy node]
  (let [data (.-data node)
        slot (.-slot data)
        offset (- slot slot-offset)]
    (+ 0 (* dy offset) (/ dy 2))))

(defn compute-fill [highest-slot offset]
  (let [slot (- highest-slot offset)]
    (if (zero? (mod slot 32))
      "#e9f5ec"
      (if (even? slot)
        "#e9ecf5"
        "#fff"))))

(defn compute-node-fill [d]
  (if (canonical-node? d)
    "#eec643"
    "#555"))

(defn compute-node-stroke [d]
  (if-let [_ (.-children d)]
    ""
    (if (canonical-node? d)
      "#d5ad2a"
      "")))

(defn draw-tree! [root width]
  (let [leaves (.leaves root)
        highest-slot (js/parseFloat (apply max (map #(-> % .-data .-slot) leaves)))
        lowest-slot (js/parseFloat (-> root .-data .-slot))
        slot-count (- highest-slot lowest-slot)
        dy (.-dy root)
        height (* dy (inc slot-count))
        svg (-> (js/d3.selectAll "#fork-choice")
                (.append "svg")
                (.attr "viewBox" (array 0 0 width height))
                (.attr "preserveAspectRatio" "xMinYMin meet")
                (.attr "class" "svg-content-responsive"))
        background (-> svg
                       (.append "g")
                       (.attr "font-size" 10)
                       )
        slot-rects (-> background
                       (.append "g")
                       (.selectAll "g")
                       (.data (clj->js (into [] (range (inc slot-count)))))
                       (.join "g")
                       (.attr "transform" #(str "translate(0 " (* dy %)")")))
        _ (-> slot-rects
                       (.append "rect")
                       (.attr "fill" #(compute-fill highest-slot %))
                       (.attr "x" 0)
                       (.attr "y" 0)
                       (.attr "width" "100%")
                       (.attr "height" dy))
        _ (-> slot-rects 
                       (.append "text")
                       (.attr "text-anchor" "start")
                       (.attr "y" (* dy 0.5))
                       (.attr "x" 5)
                       (.attr "fill" "#6c757d")
                       (.text #(slot-guide->label highest-slot %))
                       )
        g (-> svg
              (.append "g")
              (.attr "transform"
                     (str "translate(" (/ width 2) "," height ") rotate(180)")))
        links  (-> g
                   (.append "g")
                   (.attr "fill" "none")
                   (.attr "stroke"  "#555")
                   (.attr "stroke-opacity" 0.4)
                   (.attr "stroke-width" 1.5)
                   (.selectAll "path")
                   (.data (.links root))
                   (.join "path")
                   (.attr "d" (-> (js/d3.linkVertical)
                                  (.x #(.-x %))
                                  (.y #(node->y-offset lowest-slot dy %))
                                  )))

        nodes   (-> g
                    (.append "g")
                    (.selectAll "g")
                    (.data (.descendants root))
                    (.join "g")
                    (.attr "transform" #(str "translate(" (.-x %) "," (node->y-offset lowest-slot dy %)  ")")))
        _ (-> nodes
                      (.append "circle")
                      (.attr "fill" compute-node-fill)
                      (.attr "stroke" compute-node-stroke)
                      (.attr "stroke-width" 3)
                      (.attr "r" (* dy 0.2)))
        _ (-> nodes
                   (.append "text")
                   (.attr "dx" "1em")
                   (.attr "transform" "rotate(180)")
                   (.attr "text-anchor" "start")
                   (.text node->label)
                   )]))

(defn render-fork-choice! [root]
  (let [width (* (.-innerWidth js/window) (/ 9 12))
        height (.-innerHeight js/window)
        head-count (.-length (.leaves root))
        dy (* height 0.05)
        dx (/ width (+ 4 head-count))
        _ (aset root "dx" dx)
        _ (aset root "dy" dy)
        mk-tree (-> (js/d3.tree)
                    (.nodeSize (array dx dy)))
        root (mk-tree root)
        svg (js/d3.select "#fork-choice svg")]
    (empty-svg! svg)
    (draw-tree! root width)))


(defn start-polling-for-head-count []
  (fetch-head-count)
  (let [head-count-task (js/setInterval fetch-head-count polling-frequency)]
    (swap! state assoc :head-count-task head-count-task)))

(defn refresh-fork-choice []
  (go (let [response (<! (http/get (url-for "fork-choice")
                                   {:with-credentials? false}))
            fork-choice (js/d3.hierarchy (clj->js (:body response)))]
        (render-fork-choice! fork-choice))))

(defn block-for [ms-delay]
  (let [c (chan)]
    (js/setTimeout (fn [] (close! c)) ms-delay)
    c))

(defn fetch-block-tree-if-new-head [old new]
  (when (not= old new)
    (refresh-fork-choice)))

(defn fetch-heads []
  (go (let [response (<! (http/get (url-for "heads")
                                   {:with-credentials? false}))
            heads (:body response)
            [majority-root _] (process-heads-response heads)
            old-root (get @state :majority-root "")]
        (go (let [blocking-task (block-for 3000)]
              (<! blocking-task)
              (fetch-block-tree-if-new-head old-root majority-root)))
        (swap! state assoc :majority-root majority-root)
        (swap! state assoc :heads (->> heads
                                       (map (partial attach-majority majority-root))
                                       (map attach-name))))))

(defn start-polling-for-heads []
  (fetch-heads)
  (let [polling-task (js/setInterval fetch-heads polling-frequency)]
    (swap! state assoc :polling-task polling-task)))


(defn update-slot-clock []
  (let [eth2-spec (:eth2-spec @state)
        genesis-time (:genesis_time eth2-spec)
        seconds-per-slot (:seconds_per_slot eth2-spec)
        slots-per-epoch (:slots_per_epoch eth2-spec)]
    (swap! state assoc :slot-clock (calculate-eth2-time genesis-time seconds-per-slot slots-per-epoch))))

(defn start-slot-clock []
  (let [timer-task (js/setInterval update-slot-clock slot-clock-refresh-frequency)]
    (swap! state assoc :timer-task timer-task)))

(defn start-viz []
  (go (let [spec-response (fetch-spec-from-server)
            spec (:body (<! spec-response))]
        (swap! state assoc :eth2-spec spec)
        (mount)
        (start-slot-clock)
        (start-polling-for-heads)
        ;; (start-polling-for-head-count)
        (refresh-fork-choice)
        )))

(defonce init
  (start-viz))
