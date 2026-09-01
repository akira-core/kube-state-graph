# kube-state-graph

[English README](README.md)

以 Go 實作的 REST API，回傳一或多個 Kubernetes 叢集上統一的 pod／node／PVC 圖，包含可依 pod UID 對應的 RPC 邊（`pod-calls-pod`），且邊可跨叢集。

```
cluster A: kube-state-metrics ──┐
           service-graph source ┤
                                 │  (vmagent / Prometheus
cluster B: kube-state-metrics ──┤   帶 external_labels:
           service-graph source ┤   { cluster: "<name>" })
                                 │
       ...                       ├──► centralised VictoriaMetrics ◄── kube-state-graph
                                 │                                     （Prometheus HTTP API）
cluster N: kube-state-metrics ──┤
           service-graph source ─┘
```

## 功能概要

- 依呼叫端指定的 `[start, end]`，從**單一**集中式 VictoriaMetrics 讀取 `kube_*` 拓樸、Harvest／kubelet 儲存系列，以及 `traces_service_graph_*` 執行期指標。建圖會查的每一條系列列在 [`docs/upstream-metrics.md`](docs/upstream-metrics.md)。
- Join 成多叢集圖，節點鍵為帶叢集範圍的 pod UID 與 node 名稱。
- 回傳 Cytoscape.js JSON（`/v1/graph`）。
- 提供靜態邊類型目錄（`/v1/edge-types`）。有資料的叢集清單改由任一 `/v1/graph` 回應的 `clusters` 欄位提供。
- 每次請求都重新建圖——v1 **不附帶 in-process result cache、singleflight，也不發 HTTP cache validator**（無 `ETag` / `If-None-Match` / `304`）。後續分散式部署的水平擴展 cache 機制留待另案。`start` / `end` 接受 RFC 3339 或 Unix 秒，server 僅強制 `end > start`，其後原樣 pass through 給上游 PromQL——**不做** bucketing、alignment、視窗上限或未來時間擋板；bounded query cost 交由 VictoriaMetrics 搜尋限制負責。序列化輸出為確定性 body，僅含 `apiVersion`、`clusters`、`elements`；pod／node／service 的 IP 在頂層 `ipaddress`，不在 `labels`。Pod 另帶具型別的 `data` 屬性——`owner`（`{kind, name}`）、`application`（ArgoCD 應用）、`containers`（`[{name, image}]`）——皆 `omitempty` 且絕不在 `labels`。
- **在來源端收斂**：`cluster`、`namespace`、`az`、`env` 會被渲染成上游 PromQL 的 label matcher，由 VictoriaMetrics 先過濾，樣本才上線。service-graph 系列刻意完整讀取，詳見[請求過濾參數](#請求過濾參數)。

## 快速開始

```bash
make build
./bin/kube-state-graph \
  --prom-url=http://victoria-metrics.example:8428 \
  --listen-addr=:8080
```

查詢範例（`start`／`end` 為 Unix 秒，下列寫法在 macOS 與 Linux 皆可）：

```bash
end=$(date -u +%s)
start=$((end - 300))
curl "http://localhost:8080/v1/graph?start=${start}&end=${end}" | jq '.elements'

# 單一 namespace 的儲存拓樸，含無流量的工作負載。
curl "http://localhost:8080/v1/graph?start=${start}&end=${end}&namespace=payments&prune=false" | jq '.elements'

# 單一可用區／環境。
curl "http://localhost:8080/v1/graph?start=${start}&end=${end}&az=eu-west-1a&env=prod" | jq '.clusters'
```

## 請求過濾參數

`GET /v1/graph` 必填 `start`、`end`，另有六個選填參數。同一參數多值為 OR，不同參數為 AND。

| 參數 | 生效層 | 說明 |
|---|---|---|
| `cluster` | 上游 **與** projection | 可重複。`unknown` 代表未帶 `cluster` label 的系列。 |
| `namespace` | 上游 **與** projection | 可重複。收斂 pod／claim／Service／EndpointSlice 系列；node 與 NetApp aggregate 則**靠參照**跟著收斂。 |
| `az` | 上游 | 可重複。比對 `--az-label`（預設 `az`），套用於所有拓樸查詢。 |
| `env` | 上游 | 可重複。比對 `--env-label`（預設 `env`）。 |
| `edge_type` | projection | 可重複，依 `/v1/edge-types` 驗證。 |
| `prune` | projection | `true`（預設）只保留位於 connectivity edge 上的工作負載；`false` 回傳完整清單：所有已載入 pod 及其 node／PVC／NetApp 鏈，且在未帶 `cluster`／`namespace` 時連未被參照的基礎設施也一併列出。 |

**哪個 matcher 進到哪條系列**是硬編碼契約：

| 系列 | `az` | `env` | `cluster` | `namespace` |
|---|---|---|---|---|
| pod／claim／Service／EndpointSlice KSM 系列、kubelet volume stats | ✅ | ✅ | ✅ | ✅ |
| `kube_node_*` | ✅ | ✅ | ✅ | —（無此 label） |
| NetApp Harvest（`volume_labels`、`qos_*`、`aggr_*`、`node_new_status`） | ✅ | ✅ | —（其 `cluster` 是 **ONTAP** 叢集） | — |
| `traces_service_graph_*`、`up` | — | — | — | — |

service-graph 系列**每次請求都完整讀取**：其 `cluster` label 常缺漏且來自 trace 來源端，namespace label 也只描述呼叫端自身視角，在此收斂會丟掉已載入拓樸仍需要的邊。取而代之，帶過濾參數的建圖套用兩條規則：

- endpoint 的 pod **未載入**時，比照 UID 為空處理——`"://"` label 仍可解析到已載入的 Service，其餘一律成為 `external/<label>`（`labels` 為空），且**永不產生 synthesised pod**；
- 一條系列至少要有**一端**觸及已載入拓樸才會被採用，範圍外的其他部分因此不會渲染成 external 節點網。

可見後果：帶 `?cluster=` 或 `?namespace=` 時，過濾範圍外的 peer 會以 `external` 節點呈現而非真實 pod——不必載入其餘估整體，也能看見該範圍的進出相依。

> **部署前提**：所有拓樸系列族（kube-state-metrics、kubelet、Harvest）都必須帶上所設定的 `az` / `env` label。缺少者在該過濾條件下不會匹配到任何資料；又因預設 projection 只保留有連線的工作負載，缺 label 可能讓帶過濾參數的請求回傳空圖而非部分圖。

## 上游指標

完整營運者目錄（全部 **41** 條系列、PromQL 包裝、固定 selector、query 失敗 vs 空向量、每次請求的 fan-out）見 [`docs/upstream-metrics.md`](docs/upstream-metrics.md)（英文）。

摘要：一次 `/v1/graph` 會平行發出 **37** 條拓樸查詢（20 條 kube-state-metrics 遇 query 錯誤會失敗整次建圖；2 條基數隨歷史累積的 annotation 族為 log-and-continue；13 條 Harvest + 2 條 kubelet 為 log-and-continue），再發 **3** 條 service-graph 查詢（帶過濾且未載入任何 pod／service 時整組跳過），`up{}` 只在未過濾且拓樸為空時探測。**沒有 metric-name prefix**，一律用裸名查詢。v1 無結果快取。

K8s 形狀的系列預期帶有由 `vmagent`／Prometheus `external_labels` 寫入的 `cluster` 標籤。Harvest 的 `cluster` 是 **ONTAP** 叢集，不用來當 `?cluster=`。

下表「必填？」指的是**空向量**會不會丟掉該功能。kube-state-metrics 20 條 abort-on-error 或 `traces_service_graph_request_total` 的 **query 錯誤**（逾時／5xx）會失敗整次建圖；`kube_replicaset_annotations` 與 `kube_job_annotations`（基數隨歷史累積、不是活物件數）、Harvest、kubelet 與兩條 RED 系列則 log-and-continue。細節見目錄。

### 拓樸指標 — 由 [`kube-state-metrics`](https://github.com/kubernetes/kube-state-metrics) 產出

| 指標 | 用途 | 會讀的標籤 | 必填？ |
|---|---|---|---|
| `kube_pod_info` | Pod 節點（`node` 標籤驅動 `pod-to-node` 邊；pod 落在 `cluster > namespace > application > controller > pod` 工作負載階層） | `cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip`（→ `data.ipaddress`；不匯出 `host_ip`） | **是** |
| `kube_node_info` | K8s node 節點 | `cluster`, `node` | **是** |
| `kube_node_status_addresses{type=~"ExternalIP\|InternalIP"}` | Node `data.ipaddress`——優先 ExternalIP，沒有時退回 InternalIP | `cluster`, `node`, `type`, `address` | 選填（缺則無 `ipaddress`） |
| `kube_node_status_condition{condition="Ready"}` | Node Ready 狀態 `data.ready_status` ∈ {`Ready`, `NotReady`, `Unknown`}，取 active（`status` 值為 1）那列；無 Ready 資料則省略——與 `Unknown`（kubelet 失聯）有別 | `cluster`, `node`, `condition`, `status` | 選填（缺則無 `data.ready_status`）；屬 KSM 預設 |
| `kube_node_labels` | 傳遞 node 標籤（`kubernetes.io/*` 等） | `cluster`, `node`, `label_*` | 選填 |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | PVC 節點、`pod-mounts-pvc` 邊 | `cluster`, `namespace`, `pod`, `persistentvolumeclaim`, `volume` | 選填（無 PVC 則無相關節點／邊） |
| `kube_persistentvolumeclaim_info` | PVC `data.storageclass`（policy 名稱，不是節點）+ `labels.volumename`（bound PV 名；Harvest join 鍵） | `cluster`, `namespace`, `persistentvolumeclaim`, `storageclass`, `volumename` | 選填（缺則無 `data.storageclass`／`volumename`，也無法 Harvest join） |
| `kube_pod_owner` | Pod controller-owner `data.owner` = `{kind, name}`（ReplicaSet 上溯至 Deployment；無 controller 則省略）。解析出的 owner 同時是該 pod ArgoCD Application 的 join 鍵，兩者一起驅動工作負載階層的 `application`／`controller` compound group | `cluster`, `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller` | 選填（缺則無 `data.owner`，也無 `data.application`——Application 以 controller 為鍵） |
| `kube_replicaset_owner` | 把 ReplicaSet 擁有者上溯到 Deployment | `cluster`, `namespace`, `replicaset`, `owner_kind`, `owner_name` | 選填（缺則 owner 停在 ReplicaSet） |
| `kube_job_owner{owner_kind="CronJob",owner_is_controller="true"}` | 把 Job 上溯到 CronJob，**僅供 pod ArgoCD Application 解析**——Kubernetes CronJob controller 只把 `spec.jobTemplate.metadata` 的 annotation 複製到它建立的 Job，ArgoCD 的 tracking-id 因此永遠到不了 Job。不影響 `data.owner` | `cluster`, `namespace`, `job_name`, `owner_kind`, `owner_name`, `owner_is_controller` | 選填（缺則 CronJob 管理的 pod 無 `data.application`）；屬 KSM 預設 |
| `kube_{deployment,statefulset,daemonset,replicaset,job,cronjob}_annotations{annotation_argocd_argoproj_io_tracking_id!=""}` | Pod 的 ArgoCD Application `data.application`（tracking-id 首個 `:` 前的片段），以 `(cluster, namespace, kind, name)` join 到該 pod 解析出的 controller owner——ArgoCD 把 annotation 蓋在它套用的 workload 物件上，不會蓋在 controller 生出的 pod 上。並把 pod 放進 `application` compound group | `cluster`、`namespace`、各族的識別標籤（`deployment`／`statefulset`／`daemonset`／`replicaset`／**`job_name`**／`cronjob`）、`annotation_argocd_argoproj_io_tracking_id` | **逐族**選填（缺則該 controller 種類的 pod 無 `data.application`）。每族各自**需要** `--metric-annotations-allowlist=<複數資源名>=[argocd.argoproj.io/tracking-id]`（非 KSM 預設）。遇 **query 錯誤**時 `replicaset`／`job` 兩族 log-and-continue（基數隨歷史累積），其餘四族失敗整次建圖 |
| `kube_pod_container_info` | Pod 容器清單 `data.containers` = `[{name, image}]`，依 `(name, image)` 排序；視窗內換 image 時每容器取最新觀測到的 image | `cluster`, `namespace`, `pod`, `container`, `image` | 選填（缺則無 `data.containers`）；屬 KSM 預設 |
| `kube_service_info` | `://` 連線字串解析用的 Service 索引（D29）；`cluster_ip`（headless `None` ⇒ 無 `data.ipaddress`） | `cluster`, `namespace`, `service`, `cluster_ip` | 選填（缺則 `://` endpoint 退成 `external`） |
| `kube_service_annotations` | Service 的 ArgoCD Application `data.application`（tracking-id 第一個 `:` 前的片段），並把 service 放進 `application` compound group | `cluster`, `namespace`, `service`, `annotation_argocd_argoproj_io_tracking_id` | 選填（缺則無 `data.application`）。**需要** `--metric-annotations-allowlist=services=[argocd.argoproj.io/tracking-id]`（非 KSM 預設） |
| `kube_persistentvolumeclaim_annotations` | PVC 自己的 ArgoCD Application `data.application`（解析同 service）。沒有 annotation 的 PVC 會再**繼承**掛載它的 pod 之中詞彙最小的 Application | `cluster`, `namespace`, `persistentvolumeclaim`, `annotation_argocd_argoproj_io_tracking_id` | 選填（缺則無自己的 annotation；繼承仍可能填上 `data.application`）。**需要** `--metric-annotations-allowlist=persistentvolumeclaims=[argocd.argoproj.io/tracking-id]`（非 KSM 預設） |
| `kube_endpointslice_endpoints` | Service → 後端 pod fan-out（`service-selects-pod` 邊） | `cluster`, `namespace`, `endpointslice`, `targetref_kind`, `targetref_namespace`, `targetref_name` | 選填 |
| `kube_endpointslice_labels` | 把 EndpointSlice join 到所屬 Service | `cluster`, `namespace`, `endpointslice`, `label_kubernetes_io_service_name` | 選填——**需要** `--metric-labels-allowlist=endpointslices=[kubernetes.io/service-name]`（非 KSM 預設）；缺則無法做 `service-selects-pod` 解析 |

這 22 條來自十一個 kube-state-metrics collector（`pods`、`nodes`、`services`、`persistentvolumeclaims`、`replicasets`、`endpointslices`、`deployments`、`statefulsets`、`daemonsets`、`jobs`、`cronjobs`），只需這些 resource 的 `list` + `watch`。後五個純粹為了 pod 的 ArgoCD Application：不開它們，圖形完全不變，只是 pod 不帶 `data.application`。最小 Helm values、對應 ClusterRole，以及 `cluster`／`az`／`env` external-label 前提見 `docs/kube-state-metrics-preconditions.md`。

### Harvest + kubelet 儲存指標

| 指標 | 用途 | 會讀的標籤 | 必填？ |
|---|---|---|---|
| `volume_labels` | **Hop A — 整個儲存拓樸。** PVC→aggregate join（由 PV 名推導出的 token 比對 `volume`）、`netapp-aggr`／`netapp-node` 實體、PVC `svm` 標籤。info series：只讀 labels、丟棄 sample 值。不帶任何 request matcher | `cluster`（ONTAP 叢集）、`node`、`aggr`、`svm`、`volume` | 選填（缺則無 NetApp 節點／邊／`svm`，且完全不發出 QoS 查詢） |
| `qos_read_ops`／`qos_write_ops`／`qos_read_latency`／`qos_write_latency`／`qos_read_data`／`qos_write_data` | **Hop B — I/O**，掛在 `pvc-to-netapp-aggr`（`read_ops`、`write_ops`、`read_latency_us`、`write_latency_us`、`read_bytes_per_sec`、`write_bytes_per_sec`）。**原樣讀取**——Harvest 已把 ONTAP counter 解成 ops/s、平均 µs、bytes/s；絕不包 `rate()`。**收斂**到 hop A 對到的 FlexVol 名稱，除此之外不帶任何 matcher。LUN workload 帶著所屬 FlexVol 的 `volume`，會被抓下來（SAN backend 上它是唯一帶有 QoS policy 的 series），再由 reader 在加總前丟棄，因此不會被疊加上去 | `cluster`、`svm`、`policy_group`、`lun`、`volume` | 選填（缺則邊仍在、沒有 `metrics`） |
| `qos_policy_fixed_max_throughput_iops`／`qos_policy_fixed_max_throughput_mbps` | **Hop C — 宣告上限** `max_iops`／`max_bytes_per_sec`，join 鍵為 `(cluster, svm, policy_group)` 三元組——cluster 與 svm 來自 hop A，policy group 來自 hop B。鍵不完整或查無對應即忽略，絕不退回同 SVM 其他 policy group 的數值。`mbps` 是唯一換算值（× 1048576 → bytes/s），與 `read_bytes_per_sec` 同單位 | `cluster`、`svm`、`name`（或 `policy_group`） | 選填（缺則無上限欄位；永不為 `0`） |
| `aggr_new_status` | Aggregate `data.health`（sample 為 `1` 則 `online`，否則 `degraded`；無 series 則省略） | `cluster`、`node`、`aggr` | 選填 |
| `aggr_space_used`／`aggr_space_total` | Aggregate `data.usage` `{used_bytes, capacity_bytes}` | `cluster`、`node`、`aggr` | 選填 |
| `node_new_status` | Controller `data.health`（對應方式同 aggregate） | `cluster`、`node` | 選填 |
| `kubelet_volume_stats_used_bytes`／`kubelet_volume_stats_capacity_bytes` | PVC `data.usage` `{used_bytes, capacity_bytes}` | `cluster`、`namespace`、`persistentvolumeclaim` | 選填 |

上述標籤全部是 **stock Harvest 輸出**——不需要、也不會讀取任何 relabel 規則。ONTAP volume 名稱不允許 `-`，因此接往 Kubernetes 的橋樑由 backend 自行搭建：把 PVC 綁定的 PV 名改寫成 match token（預設把 `-` 換成 `_`），再與 `volume` 比對（預設以 suffix 比對，因此不必宣告 provisioner 的 `storagePrefix`）。兩者皆可設定。見 `docs/netapp-harvest-preconditions.md`。

各拓樸／Harvest／kubelet 指標以 `last_over_time(<metric>[<window>]) @ <end>` 包裝，反映請求視窗 `[start, end]` 內最後觀測值——惟 `kube_pod_container_info` 改用 `tlast_over_time(...)`，使每條 per-image series 帶其最後 sample 時間戳，供解析器挑出每容器最新的 image（近期視窗準確；遠期視窗的限制見 `design.md` D-A4）。

### Service graph 指標 — 由 [Tempo](https://grafana.com/docs/tempo/latest/metrics-generator/service_graphs/) 或相容產生器產出

| 指標 | 用途 | 會讀的標籤 | 必填？ |
|---|---|---|---|
| `traces_service_graph_request_total` | `pod-calls-pod`（叢集內／跨叢集）、`pod-calls-service`（可跨叢集——route engine 會錨定在選中的 ingress cluster）、`service-selects-pod`（可跨叢集）邊；`data.metrics.rate` 的分母。**query 錯誤**會失敗建圖；空向量不會 | `cluster`, `client`, `server`, `client_k8s_pod_uid`, `server_k8s_pod_uid`，以及僅在 `server="unknown"` 富化時使用的 peer-address 標籤（`client_server_address`，其次 `client_network_peer_address`，再次 `client_net_peer_name`） | 資料面選填（無 series 則無呼叫邊）；query 錯誤為致命 |
| `traces_service_graph_request_failed_total` | 已量測邊上的 `data.metrics.error_rate` | 與 `_total` 相同的 identity 標籤（扣掉 `__name__` 後精確 join） | 選填——缺漏／query 錯誤則省略 `error_rate`（永不回報 `0`） |
| `traces_service_graph_request_server_seconds_bucket` | 已量測邊上的 `data.metrics.p90_server_ms`（server 端 classic histogram） | 與 `_total` 相同的 identity 標籤，外加 `le`——**原樣**讀取、不做上游聚合，扣掉 `le` 後按 identity join | 選填——缺漏／非 classic bucket 則省略 `p90_server_ms` |
| `edge_relation`（**維度**，不是指標） | 值為 `link` 時標記 connector 從 **span-link** 做出的邊：邊仍會發出，但量的是 queue／DB hop 而非 request，因此不計入 `data.metrics` | 三條系列都會讀；兩條 RED selector 以 `edge_relation!="link"` 排除 | 選填——沒設這個維度的 producer 不受影響（負向 matcher 會保留缺此 label 的 series） |

以 `rate(traces_service_graph_request_total[<window>]) @ <end>` 評估。每條 series 帶單一 `cluster` external label，代表追蹤來源（通常是執行 Tempo metrics-generator 的 cluster），即呼叫的 **client 端** cluster。**Server 端** cluster 由 build 時把 `server_k8s_pod_uid` 對全域 topology pod-UID index join 還原——K8s pod UID 在實務上跨 cluster 唯一，lookup 可明確還原。僅在兩端都能解析時才輸出邊。當某端的 pod-UID 標籤為空時，會用內建的**連線字串判斷**（無旗標可調）解析其 `client`／`server` 人類可讀標籤：含字面 `://` 的標籤視為 URL——叢集內 `<service>.<namespace>.svc` 名稱會解析為**呼叫端自己 cluster 中的單一** `type="service"` 節點（因此**連線字串這條路徑**永遠為叢集內；route engine 那條路徑可錨定在同 family 的其他 cluster，所以該邊類型註冊為 `may_cross_cluster: true`），前提是該 cluster 持有同名 Service。該 service 節點再隨需 fan-out `service-selects-pod` 邊，指向**同 family 中每個持有同名 Service 的 cluster** 的後端 pod——因此 `service-selects-pod` **可跨叢集**，對應多叢集 service mesh 的端點聚合（cluster 名稱在壓縮數字串後相同即同 family，例如 `prod-1` ↔ `prod-2`）。headless 的 `<pod>.<service>.<namespace>.svc` 名稱會解析為**相同的** service 節點（丟棄前導 pod-hostname）——`://` endpoint 永不為特定 pod。無法解析的 URL、或呼叫端 cluster 未持有該 Service 時，則成為 `external` 節點；非 URL（不含 `://`）的標籤則經 missing pod-UID human-label fallback 亦成為 `external` 節點。

`servicegraph` connector 產生的**虛擬節點**——`client="user"`（未被 instrument 的呼叫端）與 `unknown`（無法解析的對端）——會在 query 層排除（`client!~"user|unknown",server!~"user"`），通常不會出現為任何節點或邊。比對為精確且大小寫敏感，因此 host 只是「包含」`user` 的 `://` 連線字串不受影響。**Server 端收窄為 `server!~"user"`**，因此 `server="unknown"` 的 series 仍會到達 reader：當其 client 端解析為**真實** pod、且 client 端記錄的對端位址（依序 `client_server_address`、`client_network_peer_address`、`client_net_peer_name`，第一個非空者勝出）指向某個 Kubernetes Service 時，該對端會被還原成 `pod-calls-service` 邊（非叢集位址則成為 `external` 節點），而非直接丟棄；其餘所有 `server="unknown"` 情況仍如舊 byte-for-byte 丟棄。

### 探針 — 診斷用，不屬於圖資料

| PromQL | 用途 |
|---|---|
| `up` | 支撐 `GET /readyz`，並區分「視窗內無資料」（`outside_retention`）與「上游正常但視窗為空」。僅在**未帶過濾參數**的建圖才發送——只要帶了任一過濾參數，零筆結果即代表「範圍內無資料」，回 200 空圖。不是圖資料。 |

帶過濾的建圖若未載入任何 pod 也未載入任何 service，三條 `traces_service_graph_*` 查詢會**整組跳過**——admission 不可能留下任何 series，且這三條是唯一不受請求 matcher 收斂的家族。

**不是 VictoriaMetrics 系列、也不是圖輸入：** 可選的 ClickHouse Istio route store（`--route-store-dsn`）把 global FQDN peer 解成 Service（預設關閉；miss 退成 `external`）；`kube_state_graph_*` 是 API 自己的 `/metrics` self-metrics。

### 邊類型 ↔ 指標

| 邊類型 | 來源指標 |
|---|---|
| `pod-mounts-pvc` | `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `pod-to-node` | `kube_pod_info`（`node` 標籤；每個已排程 pod 一條，叢集內） |
| `pvc-to-netapp-aggr` | Harvest `volume_labels`，以 PVC `volumename`（PV 名）推導出的 token 比對 stock `volume` 標籤 |
| `pod-calls-pod` | `traces_service_graph_request_total` |
| `pod-calls-service` | `traces_service_graph_request_total`（目標透過連線字串／peer-address／route engine 解析為 service 節點時） |
| `service-selects-pod` | `traces_service_graph_request_total`（連線字串解析 + `kube_endpointslice_*` join，隨需產生） |

### 驗證

多叢集／跨叢集／service-graph 情境由 **`internal/integration/`** 搭配 testcontainers-go 起的 VictoriaMetrics 容器涵蓋：手工製作的 fixture series 以 Prometheus 文字曝露格式（`POST /api/v1/import/prometheus`）推進容器後，再對 in-process API server 驗證。

## 設定

| 旗標 | 環境變數 | 預設值 | 說明 |
|---|---|---|---|
| `--prom-url` | `KSG_PROM_URL` | `http://localhost:8428` | VictoriaMetrics Prometheus 相容 endpoint。 |
| `--listen-addr` | `KSG_LISTEN_ADDR` | `:8080` | HTTP 監聽位址。 |
| `--build-timeout` | `KSG_BUILD_TIMEOUT` | `15s` | `/v1/graph` 的單次建圖 context 逾時。 |
| `--api-timeout` | `KSG_API_TIMEOUT` | `5s` | 建圖以外的 upstream 呼叫逾時（`/readyz` 探針、outside-retention 探針）。 |
| `--api-keys-file` | `KSG_API_KEYS_FILE` | （空） | 接受的 API key 檔案路徑（每行一個，`#` 為註解）。為 K8s `Secret` 掛載而設計，會週期性重新讀取。 |
| `--api-keys` | `KSG_API_KEYS` | （空） | 逗號分隔字面 key；僅 dev 用途，設了 `--api-keys-file` 即忽略。 |
| `--api-keys-reload-interval` | `KSG_API_KEYS_RELOAD_INTERVAL` | `30s` | `--api-keys-file` 重新讀取頻率；`0` 關閉熱重載。 |
| `--log-level` | `KSG_LOG_LEVEL` | `info` | `debug \| info \| warn \| error`。 |
| `--az-label` | `KSG_AZ_LABEL` | `az` | `?az=` 參數比對的上游 label。請求參數名稱固定不變，只有 label 綁定會改；必須是合法 PromQL label name 且與 `--env-label` 不同。 |
| `--env-label` | `KSG_ENV_LABEL` | `env` | `?env=` 參數比對的上游 label。 |

## 文件

- **上游指標目錄（全部 41 條）：** [`docs/upstream-metrics.md`](docs/upstream-metrics.md)
- **kube-state-metrics 安裝／RBAC／allowlist：** [`docs/kube-state-metrics-preconditions.md`](docs/kube-state-metrics-preconditions.md)
- **NetApp Harvest relabel 與 hops：** [`docs/netapp-harvest-preconditions.md`](docs/netapp-harvest-preconditions.md)

完整 API 參考由執行中的 server 提供：

- **互動式 API 參考（Scalar UI）：** [`/docs`](http://localhost:8080/docs)
- **OpenAPI 3.1 規格：** [`/openapi.yaml`](http://localhost:8080/openapi.yaml) · [`/openapi.json`](http://localhost:8080/openapi.json)

規格由原始碼註解產生（`make docs`）並嵌入 binary，因此永遠與執行中的 build 一致。Scalar UI 的前端 bundle 由 jsDelivr CDN 載入。

## 開發

### 第一次設定

clone 後**只跑一次**。下載 modules、安裝 host-level 工具（`golangci-lint`、
`govulncheck`）。Mockery 由 go.mod 的 `tool` directive（Go 1.24+）追蹤，透過
`go tool mockery` 呼叫，不需另外安裝。

```bash
make init           # go mod download + dev tools
make doctor         # 檢查工具版本（go、golangci-lint、govulncheck、mockery、docker）
make init-hooks     # （選用）安裝 pre-commit hook（gofmt + go vet）
```

需求：Go 1.25+。`go.mod` 中 pin 的 toolchain（目前 `go1.26.5`）會在第一次 build 時自動下載。

### 日常指令

```bash
make build          # 編譯主程式
make test           # 單元 + 元件 + golden + property + integration（需 Docker）
make lint           # golangci-lint
make vuln           # govulncheck
make check-docs     # OpenAPI 規格是否與 swag 產出一致（CI 亦跑）
```

### Mocks（mockery）

production-side 依賴透過小介面（`promql.Querier`、`auth.Validator`、`clock.Clock`）暴露，單元測試用 mockery 生成的 mock 注入，**不再用 `httptest.NewServer` 假冒上游服務**。Mock 放在 `internal/<pkg>/mocks/` 並 commit 進 git，CI 不需安裝 mockery。

```bash
make mocks          # 編輯介面後重新產生 mocks
make verify-mocks   # CI 風格的 freshness 檢查（regen + git diff）
```

`.mockery.yaml` 列出所有設定的介面。**新增或修改介面後**請執行 `make mocks` 並 commit；否則 CI 的 `mocks-drift` job 會擋下 merge。

### 測試分層

| 層級 | 位置 | 真實 I/O？ |
|---|---|---|
| Unit | `internal/{graph,build,promql,config,clock,auth,telemetry}/*_test.go` | 無 — 純 Go。 |
| Component | `internal/api/*_test.go` | 無 — 透過介面注入 `MockQuerier`；`httptest.NewServer` 只用於包裹 server-under-test，不假冒上游。 |
| Golden | `internal/api/golden_test.go` + `testdata/golden/*.json` | 無。執行 `-update` 重新生成 snapshot。 |
| Integration | `internal/integration/*` | **需 Docker。** testcontainers-go 啟動真實 VictoriaMetrics 容器；`SkipIfDockerUnavailable` 在沒有 Docker 的本機自動跳過，CI 跑全套。 |

unit 與 integration 邊界嚴格：**任何透過 TCP socket 連到上游的測試都歸類為 integration**。單元測試必須能在無外部相依下執行。整合測試走 `internal/integration/` + testcontainers-go：直接以 Prometheus 文字曝露格式把 fixture series 推進臨時 VictoriaMetrics 容器。

## 授權

Apache-2.0
