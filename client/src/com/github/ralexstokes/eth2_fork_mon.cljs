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
        slot-start-in-seconds  (+ genesis-time (* slot seconds-per-slot))
        delta (- time-in-secs slot-start-in-seconds)
        delta (if (< delta 0) (- seconds-per-slot (Math/abs delta)) delta)
        progress (* 100 (/ delta seconds-per-slot))]
    {:slot slot
     :epoch (Math/floor (/ slot slots-per-epoch))
     :slot-in-epoch (mod slot slots-per-epoch)
     :progress-into-slot progress}))

(defonce state (r/atom {:network ""}))

;; debug utility
(defn render-edn [data]
  [:pre
   (with-out-str
     (pprint/pprint data))])

(defn round-to-extremes [x]
  (let [margin 10]
    (cond
      (> x (- 100 margin)) 100
      :else x)))

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
           {:width (str (round-to-extremes progress-into-slot) "%")}}]]]])))

(defn peer-view [index {:keys [name version healthy]}]
  [:tr {:key index}
   [:th {:scope :row}
    name]
   [:td version]
   [:td {:style {:text-align "center"}}
    (if healthy
          "🟢"
          "🔴")]])

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
           [:th {:scope :col} "Version"]
           [:th {:scope :col
                 :style {:text-align "center"}} "Healthy?"]]]
         [:tbody
          (map-indexed peer-view peers)]]]]]]))

(defn humanize-hex [hex-str]
  (str (subs hex-str 2 6)
       ".."
       (subs hex-str (- (count hex-str) 4))))

(defn network->beaconchain-prefix [network]
  (case network
    "mainnet" ""
    (str network ".")))

(defn head-view [network index {:keys [name slot root is-majority?]}]
  [:tr {:class (if is-majority? :table-success :table-danger)
        :key index}
   [:th {:scope :row}
    name]
   [:td [:a {:href (str "https://"
                        (network->beaconchain-prefix network)
                        "beaconcha.in/block/"
                        slot)} slot]]
   [:td [:a {:href (str "https://"
                        (network->beaconchain-prefix network)
                        "beaconcha.in/block/"
                        (subs root 2))} (humanize-hex root)]]])

(defn compare-heads-view []
  (when-let [heads (:heads @state)]
    (let [network (:network @state)]
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
          (map-indexed #(head-view network %1 %2) heads)]]]])))

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
     [:p
      [:small
       "NOTE: nodes are labeled with their block root. Percentages are amounts of stake relative to the justified root."]]
     [:p
      [:small
       "NOTE: visualization may take a slot to synchronize."]]
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
   [:nav.navbar.navbar-expand-sm.navbar-light.bg-light
    [:a.navbar-brand {:href "#"} "eth2 fork mon"]
    [:ul.nav.nav-pills.mr-auto
     [:li.nav-item
      [:a.nav-link.active {:data-toggle :tab
                           :href "#nav-tip-monitor"} "chain monitor"]]
     [:li.nav-item
      [:a.nav-link {:data-toggle :tab
                    :href "#nav-block-tree"} "block tree"]]]
    [:div.ml-auto
     [:span.navbar-text (str "network: " (:network @state))]]]
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
  (http/get "/spec" {:with-credentials? false}))

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
      first
      str/capitalize))

(defn attach-name [peer]
  (assoc peer :name (name-from (:version peer))))

(def polling-frequency 700) ;; ms
(def slot-clock-refresh-frequency 100) ;; ms

;; (defn fetch-head-count []
;;   (go (let [response (<! (http/get "/block-tree"
;;                                    {:with-credentials? false}))
;;             response-body (:body response)
;;             head-count (:head_count response-body)]
;;         (swap! state assoc :head-count head-count))))

(defn empty-svg! [svg]
  (.remove svg))

;; NOTE: this function only labels leaves, the root and fork points with weights
;; (defn node->label
;;   "Label nodes with their root. If they are leaves or have direct siblings, add percent weight"
;;   [total-weight d]
;;   (let [data (.-data d)
;;         root (-> data .-root humanize-hex)
;;         leaf? (= 0 (count (.-children d)))
;;         suffix (if-let [parent (.-parent d)] ;; add weight to fork points
;;                  (if-let [siblings (.-children parent)]
;;                    (if (> (.-length siblings) 1)
;;                      (.-weight data)
;;                      "") ;; no siblings
;;                    "") ;; default
;;                  (.-weight data)) ;; root of tree
;;         suffix (if (and leaf?
;;                         (= "" suffix))
;;                  (.-weight data)
;;                  suffix)]
;;     (if (= suffix "")
;;       root
;;       (if (zero? suffix)
;;         (str root ", 0%")
;;         (str root ", " (-> (/ suffix total-weight)
;;                            (* 100)
;;                            (.toFixed 4)) "%")))))

(defn node->label [total-weight d]
  (let [data (.-data d)
        root (-> data .-root humanize-hex)
        weight (.-weight data)
        weight-fraction (if (zero? total-weight) 0 (/ weight total-weight))]
    (str root ", " (-> weight-fraction (* 100) (.toFixed 2)) "%")))

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

(defn node->block-explorer-link [d]
  (str "https://"
       (network->beaconchain-prefix (:network @state))
       "beaconcha.in/block/"
       (-> d
           .-data
           .-root
           (subs 2))))
  

(defn draw-tree! [root width total-weight]
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
                    (.attr "transform" #(str "translate(" (.-x %) "," (node->y-offset lowest-slot dy %)  ")"))
                    (.append "a")
                    (.attr "href" node->block-explorer-link))
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
                   (.text (partial node->label total-weight))
                   )]))

(defn render-fork-choice! [root total-weight]
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
    (draw-tree! root width total-weight)))


;; (defn start-polling-for-head-count []
;;   (fetch-head-count)
;;   (let [head-count-task (js/setInterval fetch-head-count polling-frequency)]
;;     (swap! state assoc :head-count-task head-count-task)))

(defn refresh-fork-choice []
  (go (let [response (<! (http/get "/fork-choice"
                                   {:with-credentials? false}))
            block-tree (get-in response [:body :block_tree])
            total-weight (get-in response [:body :total_weight])
            fork-choice (js/d3.hierarchy (clj->js block-tree))]
        (render-fork-choice! fork-choice total-weight))))

(defn block-for [ms-delay]
  (let [c (chan)]
    (js/setTimeout (fn [] (close! c)) ms-delay)
    c))

(defn fetch-block-tree-if-new-head [old new]
  (when (not= old new)
    (refresh-fork-choice)))

(defn fetch-heads []
  (go (let [response (<! (http/get "heads"
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
        (swap! state assoc :network (:network spec))
        (mount)
        (start-slot-clock)
        (start-polling-for-heads)
        (refresh-fork-choice)
        )))

(defonce init
  (start-viz))
