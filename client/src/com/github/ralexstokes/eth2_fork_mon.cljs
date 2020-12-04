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
(def polling-frequency 700) ;; ms
(def slot-clock-refresh-frequency 100) ;; ms

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

(defn humanize-hex [hex-str]
  (str (subs hex-str 2 6)
       ".."
       (subs hex-str (- (count hex-str) 4))))

(defn network->beaconchain-prefix [network]
  (case network
    "mainnet" ""
    (str network ".")))

(defonce state (r/atom {:network ""
                        :justified-checkpoint {:epoch 0 :root "0x0000000000000000000000000000000000000000000000000000000000000000"}
                        :finalized-checkpoint {:epoch 0 :root "0x0000000000000000000000000000000000000000000000000000000000000000"}}))

(defn render-edn [data]
  [:pre
   (with-out-str
     (pprint/pprint data))])

(defn debug-view []
  [:div.row.debug
   (render-edn @state)])

(defn round-to-extremes [x]
  (let [margin 10]
    (cond
      (> x (- 100 margin)) 100
      :else x)))

(defn clock-view []
  (when-let [eth2-spec (:eth2-spec @state)]
    (let [{:keys [slots_per_epoch]} eth2-spec
          slots-per-epoch slots_per_epoch
          network (:network @state)
          network-prefix (network->beaconchain-prefix network)
          {:keys [slot epoch slot-in-epoch progress-into-slot]} (:slot-clock @state)
          justified (:justified-checkpoint @state)
          finalized (:finalized-checkpoint @state)
          head-root (get @state :majority-root "")]
      [:div#chain-drawer.accordion
       [:div.card
        [:div.card-header
         [:button.btn.btn-link.btn-block.text-left {:type :button
                                                    :data-toggle "collapse"
                                                    :data-target "#collapseChain"}
          "Chain"]] 
        [:div#collapseChain.collapse.show {:data-parent "#chain-drawer"}
         [:div.card-body
          [:div.mb-3 "Epoch: " [:a {:href (str "https://" network-prefix "beaconcha.in/epoch/" epoch)} epoch] " (slot: " [:a {:href (str "https://" network-prefix "beaconcha.in/block/" slot)} slot] ")"]
          [:div.mb-3 (str "Slot in epoch: " slot-in-epoch " / " slots-per-epoch)]
          [:div.mb-3
           "Progress through slot:"
           [:div.progress
            [:div.progress-bar
             {:style
              {:width (str (round-to-extremes progress-into-slot) "%")}}]]]
          [:div.mb-3
           "Canonical head root: "
           [:a {:href (str "https://" network-prefix "beaconcha.in/block/" head-root)} (humanize-hex head-root)]]
          [:div.mb-3 "Justified checkpoint: epoch "
           [:a {:href (str "https://" network-prefix "beaconcha.in/epoch/" (:epoch justified))} (:epoch justified)]
           " with root "
           [:a {:href (str "https://" network-prefix "beaconcha.in/block/" (:root justified))} (-> justified :root humanize-hex)]]
          [:div "Finalized checkpoint: epoch "
           [:a {:href (str "https://" network-prefix "beaconcha.in/epoch/" (:epoch finalized))} (:epoch finalized)]
           " with root "
           [:a {:href (str "https://" network-prefix "beaconcha.in/block/" (:root finalized))} (-> finalized :root humanize-hex)]]]]]])))

(defn peer-view [index {:keys [name version healthy syncing]}]
  [:tr {:key index}
   [:th {:scope :row}
    name]
   [:td version]
   [:td {:style {:text-align "center"}}
    (if healthy
          "🟢"
          "🔴")]
   [:td {:style {:text-align "center"}}
    (if syncing
          "Yes"
          "No")]])

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
                 :style {:text-align "center"}} "Healthy?"]
           [:th {:scope :col
                 :style {:text-align "center"}} "Syncing?"]]]
         [:tbody
          (map-indexed peer-view peers)]]]]]]))

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

(defn tree-view []
  [:div.card
   [:div.card-header
    "Block tree over last 4 epochs"]
   [:div.card-body
    [:div#head-count-viewer
     [:p
      [:small
       "NOTE: nodes are labeled with their block root. Percentages are amounts of stake attesting to a block relative to the finalized block."]]
     [:div#fork-choice.svg-container]]]])

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

(defn empty-svg! [svg]
  (.remove svg))

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
        _  (-> g
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


(defn refresh-fork-choice []
  (go (let [response (<! (http/get "/fork-choice"
                                   {:with-credentials? false}))
            block-tree (get-in response [:body :block_tree])
            total-weight (:weight block-tree)
            fork-choice (js/d3.hierarchy (clj->js block-tree))]
        (render-fork-choice! fork-choice total-weight))))

(defn block-for [ms-delay]
  (let [c (chan)]
    (js/setTimeout (fn [] (close! c)) ms-delay)
    c))

(defn fetch-block-tree-if-new-head [old new]
  (when (not= old new)
    (refresh-fork-choice)))

(defn fetch-monitor-state []
  (go (let [response (<! (http/get "/chain-monitor"
                                   {:with-credentials? false}))
            heads (get-in response [:body :nodes])
            justified (get-in response [:body :justified_checkpoint])
            finalized (get-in response [:body :finalized_checkpoint])
            [majority-root _] (process-heads-response heads)
            old-root (get @state :majority-root "")]
        ;; NOTE: we block here to give the backend time to compute
        ;; the updated fork choice... should be able to improve
        (go (let [blocking-task (block-for 700)]
              (<! blocking-task)
              (fetch-block-tree-if-new-head old-root majority-root)))
        (swap! state assoc :justified-checkpoint justified)
        (swap! state assoc :finalized-checkpoint finalized)
        (swap! state assoc :majority-root majority-root)
        (swap! state assoc :heads (->> heads
                                       (map (partial attach-majority majority-root))
                                       (map attach-name))))))

(defn start-polling-for-heads []
  (fetch-monitor-state)
  (let [polling-task (js/setInterval fetch-monitor-state polling-frequency)]
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

(defn push-hash [e]
  (.pushState js/history (clj->js {}) "" (-> e .-target .-hash)))

(defn install-navigation []
  (-> (js/$ "a[data-toggle=\"tab\"]")
      (.on "shown.bs.tab" push-hash)))

(defn restore-last-navigation []
  (let [hash (-> js/document .-location .-hash)]
    (when (not (= "" hash))
      (-> (js/$ (str ".nav a[href=\"" (str/replace hash #"tab_" "") "\"]"))
          (.tab "show")))))

(defn start-viz []
  (go (let [spec-response (fetch-spec-from-server)
            spec (:body (<! spec-response))]
        (swap! state assoc :eth2-spec spec)
        (swap! state assoc :network (:network spec))
        (mount)
        (install-navigation)
        (start-slot-clock)
        (start-polling-for-heads)
        (refresh-fork-choice)
        (restore-last-navigation)
        )))

(defonce init
  (start-viz))
