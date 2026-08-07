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

- 依呼叫端指定的 `[start, end]`，從**單一**集中式 VictoriaMetrics 讀取 `kube_*` 拓樸與 `traces_service_graph_*` 執行期指標。
- Join 成多叢集圖，節點鍵為帶叢集範圍的 pod UID 與 node 名稱。
- 回傳 Cytoscape.js JSON（`/v1/graph`）。
- 提供叢集探索（`/v1/clusters`）與靜態邊類型目錄（`/v1/edge-types`）。
- 每次請求都重新建圖——v1 **不附帶 in-process result cache、singleflight，也不發 HTTP cache validator**（無 `ETag` / `If-None-Match` / `304`）。後續分散式部署的水平擴展 cache 機制留待另案。`start` / `end` 接受 RFC 3339 或 Unix 秒，server 僅強制 `end > start`，其後原樣 pass through 給上游 PromQL——**不做** bucketing、alignment、視窗上限或未來時間擋板；bounded query cost 交由 VictoriaMetrics 搜尋限制負責。序列化輸出為確定性 body，僅含 `apiVersion`、`clusters`、`elements`；pod／node／service 的 IP 在頂層 `ipaddress`，不在 `labels`。Pod 另帶具型別的 `data` 屬性——`owner`（`{kind, name}`）、`application`（ArgoCD 應用）、`containers`（`[{name, image}]`）——皆 `omitempty` 且絕不在 `labels`。

## 快速開始

```bash
make build
./bin/kube-state-graph \
  --prom-url=http://victoria-metrics.example:8428 \
  --listen-addr=:8080
```

查詢範例（`start`／`end` 為 Unix 秒，下列寫法在 macOS 與 Linux 皆可）：

```bash
curl 'http://localhost:8080/v1/clusters'
end=$(date -u +%s)
start=$((end - 300))
curl "http://localhost:8080/v1/graph?start=${start}&end=${end}" | jq '.elements'
```

## 上游指標

每次請求都會對集中式 VictoriaMetrics 發出 PromQL（v1 無結果快取）。各 series 預期帶有由 `vmagent`／Prometheus `external_labels` 寫入的 `cluster` 標籤。

### 拓樸指標 — 由 [`kube-state-metrics`](https://github.com/kubernetes/kube-state-metrics) 產出

| 指標 | 用途 | 會讀的標籤 | 必填？ |
|---|---|---|---|
| `kube_pod_info` | Pod 節點（`node` 標籤驅動 Cytoscape compound nesting） | `cluster`, `namespace`, `pod`, `uid`, `node`, `pod_ip`（→ `data.ipaddress`；不匯出 `host_ip`） | **是** |
| `kube_node_info` | K8s node 節點 | `cluster`, `node` | **是** |
| `kube_node_status_addresses{type="ExternalIP"}` | Node 外部 IP（→ `data.ipaddress`） | `cluster`, `node`, `address` | 選填 |
| `kube_node_status_condition{condition="Ready"}` | Node Ready 狀態 `data.ready_status` ∈ {`Ready`, `NotReady`, `Unknown`}，取 active（`status` 值為 1）那列；無 Ready 資料則省略——與 `Unknown`（kubelet 失聯）有別 | `cluster`, `node`, `condition`, `status` | 選填（缺則無 `data.ready_status`）；屬 KSM 預設 |
| `kube_node_labels` | 傳遞 node 標籤（`kubernetes.io/*` 等） | `cluster`, `node`, `label_*` | 選填 |
| `kube_pod_spec_volumes_persistentvolumeclaims_info` | PVC 節點、`pod-mounts-pvc` 邊 | `cluster`, `namespace`, `pod`, `persistentvolumeclaim`, `volume` | 選填（無 PVC 則無相關節點／邊） |
| `kube_pod_owner` | Pod controller-owner `data.owner` = `{kind, name}`（ReplicaSet 上溯至 Deployment；無 controller 則省略）；並經 `argocd_tracking_id` 標籤產生 `data.application`（取首個 `:` 前的片段） | `cluster`, `namespace`, `pod`, `owner_kind`, `owner_name`, `owner_is_controller`, `argocd_tracking_id` | 選填（缺則無 `data.owner`）。`argocd_tracking_id` 為**營運者提供**（如 `--metric-labels-allowlist`／relabel），非 KSM 預設；缺則無 `data.application` |
| `kube_pod_container_info` | Pod 容器清單 `data.containers` = `[{name, image}]`，依 `(name, image)` 排序；視窗內換 image 時每容器取最新觀測到的 image | `cluster`, `namespace`, `pod`, `container`, `image` | 選填（缺則無 `data.containers`）；屬 KSM 預設 |

各指標以 `last_over_time(<metric>[<window>]) @ <end>` 包裝，反映請求視窗 `[start, end]` 內最後觀測值——惟 `kube_pod_container_info` 改用 `tlast_over_time(...)`，使每條 per-image series 帶其最後 sample 時間戳,供解析器挑出每容器最新的 image（近期視窗準確；遠期視窗的限制見 `design.md` D-A4）。

> 註：上表為精簡版,尚缺數個既有指標列（`kube_replicaset_owner`、`kube_persistentvolumeclaim_info`、`kube_service_info`、`kube_endpointslice_*`）——完整清單以英文 [README.md](README.md) 為準。

### Service graph 指標 — 由 [Tempo](https://grafana.com/docs/tempo/latest/metrics-generator/service_graphs/) 或相容產生器產出

| 指標 | 用途 | 會讀的標籤 | 必填？ |
|---|---|---|---|
| `traces_service_graph_request_total` | `pod-calls-pod`（叢集內／跨叢集）、`pod-calls-service`（叢集內）、`service-selects-pod`（可跨叢集）邊 | `cluster`, `client`, `server`, `client_k8s_pod_uid`, `server_k8s_pod_uid` 等 | 選填（無 series 則無呼叫邊） |

以 `rate(traces_service_graph_request_total[<window>]) @ <end>` 評估。每條 series 帶單一 `cluster` external label，代表追蹤來源（通常是執行 Tempo metrics-generator 的 cluster），即呼叫的 **client 端** cluster。**Server 端** cluster 由 build 時把 `server_k8s_pod_uid` 對全域 topology pod-UID index join 還原——K8s pod UID 在實務上跨 cluster 唯一，lookup 可明確還原。僅在兩端都能解析時才輸出邊。當某端的 pod-UID 標籤為空時，會用內建的**連線字串判斷**（無旗標可調）解析其 `client`／`server` 人類可讀標籤：含字面 `://` 的標籤視為 URL——叢集內 `<service>.<namespace>.svc` 名稱會解析為**呼叫端自己 cluster 中的單一** `type="service"` 節點（因此 `pod-calls-service` 永遠為叢集內），前提是該 cluster 持有同名 Service。該 service 節點再隨需 fan-out `service-selects-pod` 邊，指向**同 family 中每個持有同名 Service 的 cluster** 的後端 pod——因此 `service-selects-pod` **可跨叢集**，對應多叢集 service mesh 的端點聚合（cluster 名稱在壓縮數字串後相同即同 family，例如 `prod-1` ↔ `prod-2`）。headless 的 `<pod>.<service>.<namespace>.svc` 名稱會解析為**相同的** service 節點（丟棄前導 pod-hostname）——`://` endpoint 永不為特定 pod。無法解析的 URL、或呼叫端 cluster 未持有該 Service 時，則成為 `external` 節點；非 URL（不含 `://`）的標籤則經 missing pod-UID human-label fallback 亦成為 `external` 節點。

`servicegraph` connector 產生的**虛擬節點**——`client="user"`（未被 instrument 的呼叫端）與 `unknown`（無法解析的對端）——會在 query 層排除（`client!~"user|unknown",server!~"user"`），通常不會出現為任何節點或邊。比對為精確且大小寫敏感，因此 host 只是「包含」`user` 的 `://` 連線字串不受影響。**Server 端收窄為 `server!~"user"`**，因此 `server="unknown"` 的 series 仍會到達 reader：當其 client 端解析為**真實** pod、且 client 端記錄的對端位址（`client_net_peer_name`／`client_server_address`）指向某個 Kubernetes Service 時，該對端會被還原成 `pod-calls-service` 邊（非叢集位址則成為 `external` 節點），而非直接丟棄；其餘所有 `server="unknown"` 情況仍如舊 byte-for-byte 丟棄。

### 探針 — 診斷用，不屬於圖資料

| PromQL | 用途 |
|---|---|
| `group by (cluster) (last_over_time(kube_node_info[1h]))` | 驅動 `GET /v1/clusters` |
| `up` | 區分「視窗內無資料」（`outside_retention`）與「上游正常但視窗為空」 |

### 邊類型 ↔ 指標

| 邊類型 | 來源指標 |
|---|---|
| `pod-mounts-pvc` | `kube_pod_spec_volumes_persistentvolumeclaims_info` |
| `pod-calls-pod` | `traces_service_graph_request_total` |
| `pod-calls-service` | `traces_service_graph_request_total`（目標透過連線字串解析為 service 節點時） |
| `service-selects-pod` | `traces_service_graph_request_total`（連線字串解析隨需產生） |

pod→node 關係**不**以邊表達，而是由 Cytoscape compound nesting（`cluster > node > pod`，依每個 pod 的 `labels.node` 推導）呈現；K8s `node` 節點本身無邊，純粹作為 compound 容器（仍在 `ipaddress` 屬性帶 `external_ip`）。因此鎖定某 pod 的 `name` 過濾或 `root` 遍歷**不會**再帶入其宿主 K8s node；而 `?namespace=` 過濾會**排除** K8s node（無 namespace 標籤、無邊），namespace 收斂後的視圖僅含具名空間實體。

### 驗證

多叢集／跨叢集／service-graph 情境由 **`internal/integration/`** 搭配 testcontainers-go 起的 VictoriaMetrics 容器涵蓋：手工製作的 fixture series 以 Prometheus 文字曝露格式（`POST /api/v1/import/prometheus`）推進容器後，再對 in-process API server 驗證。

## 設定

| 旗標 | 環境變數 | 預設值 | 說明 |
|---|---|---|---|
| `--prom-url` | `KSG_PROM_URL` | `http://localhost:8428` | VictoriaMetrics Prometheus 相容 endpoint。 |
| `--listen-addr` | `KSG_LISTEN_ADDR` | `:8080` | HTTP 監聽位址。 |
| `--build-timeout` | `KSG_BUILD_TIMEOUT` | `15s` | `/v1/graph` 的單次建圖 context 逾時。 |
| `--api-timeout` | `KSG_API_TIMEOUT` | `5s` | 非 graph 端點的 upstream 呼叫逾時（`/v1/clusters`、`/readyz`）。 |
| `--api-keys-file` | `KSG_API_KEYS_FILE` | （空） | 接受的 API key 檔案路徑（每行一個，`#` 為註解）。為 K8s `Secret` 掛載而設計，會週期性重新讀取。 |
| `--api-keys` | `KSG_API_KEYS` | （空） | 逗號分隔字面 key；僅 dev 用途，設了 `--api-keys-file` 即忽略。 |
| `--api-keys-reload-interval` | `KSG_API_KEYS_RELOAD_INTERVAL` | `30s` | `--api-keys-file` 重新讀取頻率；`0` 關閉熱重載。 |
| `--log-level` | `KSG_LOG_LEVEL` | `info` | `debug \| info \| warn \| error`。 |
| `--metric-prefix` | `KSG_METRIC_PREFIX` | （空） | 附加在拓樸 reader 查詢的 kube-state-metrics 系列名稱前的前綴（例如 `o11y_` → `o11y_kube_pod_info`）。**不**影響 `traces_service_graph_request_total` 或 `up{}`。metric 名稱字尾與每條 series 的標籤集，是任何相容 exporter 都必須遵守的固定合約。 |

## 文件

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
