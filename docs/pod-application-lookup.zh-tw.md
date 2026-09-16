# 以 Pod 查詢 ArgoCD Application（`build.ResolvePodApplication`）使用說明

> 適用範圍：`feat/pod-application-lookup` branch 新增的
> `build.ResolvePodApplication`。本文件只涵蓋「給定一個 pod，查它屬於哪個
> ArgoCD Application」這個 Go API，不涵蓋 `/v1/graph` 的批次解析。
>
> 規格來源：`openspec/specs/pod-application-lookup/spec.md`；設計決策：
> `openspec/changes/archive/2026-09-16-add-pod-application-lookup/design.md`；
> 實作：`pkg/build/podapplication.go`。

---

## 1. 這個功能做什麼

`build.ResolvePodApplication` 會針對**一個**已知名稱的 pod，透過
`promql.Router.QueryLabels` 對上游 VictoriaMetrics 發出 2～4 條（依 owner
種類而定）循序的 `ksm` family label query，算出該 pod 的 ArgoCD Application
名稱。

- **不需要跑整個 graph build**，也不需要 HTTP server。
- 解析規則與 graph build 為 `PodNode.Application()` 套用的規則**完全相同**
  （兩條路徑共用同一份程式碼：`resolveOwnerApplication`、`ownerLess`、
  `betterTrackingID`、`controllerAnnotationFamilies`），所以兩邊對同一份上游
  資料得到的結果一致（由 `TestResolvePodApplication_MatchesBatchResolver` 釘住）。
- 與 build 不同：**任何一條上游 query 失敗，整個查詢就失敗**，不會降級成空值。

---

## 2. 需要 import 的 package

| Package | 用途 |
|---|---|
| `github.com/akira-core/kube-state-graph/pkg/build` | `ResolvePodApplication`、`PodApplicationRequest`、`ErrAmbiguousPod`、`LabelQuerier` 介面 |
| `github.com/akira-core/kube-state-graph/pkg/promql` | `NewRouter`、`SingleBackendTable`、`LabelKeys`（替換 az/env label 名稱時用） |
| `github.com/akira-core/kube-state-graph/pkg/promql/backendsfile` | （選用）從 routing file 讀多 backend 路由表、熱重載 |
| `github.com/akira-core/kube-state-graph/pkg/build/mocks` | （選用，測試用）mockery 產生的 `MockLabelQuerier` |

注意：

- `pkg/*` 不 import 任何 `internal/*`，外部 Go module 可以直接引用。
- `pkg/build` **不會**把 `pkg/route`（istio / ClickHouse）拉進你的 binary。
- `ResolvePodApplication` 需要的是 `build.LabelQuerier`，**不是**
  `promql.Querier`。目前唯一的正式實作是 `*promql.Router`；單純的
  `*promql.Client` 沒有 `QueryLabels`，不能直接傳進去。

```go
import (
    "context"
    "errors"
    "time"

    "github.com/akira-core/kube-state-graph/pkg/build"
    "github.com/akira-core/kube-state-graph/pkg/promql"
    // 多 backend 時才需要：
    // "github.com/akira-core/kube-state-graph/pkg/promql/backendsfile"
)
```

---

## 3. 前置條件（上游資料）

這個 API 只讀 kube-state-metrics（KSM）的 series，全部走 `ksm` family：

| Series | 用途 | 需要額外設定？ |
|---|---|---|
| `kube_pod_owner` | pod 的 controller owner | KSM 預設就有 |
| `kube_replicaset_owner` | ReplicaSet → Deployment | KSM 預設就有 |
| `kube_job_owner` | Job → CronJob | KSM 預設就有 |
| `kube_deployment_annotations` | Deployment 的 tracking-id | 需 `--metric-annotations-allowlist` |
| `kube_statefulset_annotations` | StatefulSet 的 tracking-id | 同上 |
| `kube_daemonset_annotations` | DaemonSet 的 tracking-id | 同上 |
| `kube_replicaset_annotations` | 無 Deployment 的裸 ReplicaSet | 同上 |
| `kube_job_annotations` | Job 的 tracking-id | 同上 |
| `kube_cronjob_annotations` | CronJob 的 tracking-id | 同上 |

KSM allowlist 範例（只開你需要的 resource 即可，per-resource 生效）：

```text
--metric-annotations-allowlist=deployments=[argocd.argoproj.io/tracking-id],statefulsets=[argocd.argoproj.io/tracking-id],daemonsets=[argocd.argoproj.io/tracking-id],replicasets=[argocd.argoproj.io/tracking-id],jobs=[argocd.argoproj.io/tracking-id],cronjobs=[argocd.argoproj.io/tracking-id]
```

KSM 會把 annotation 轉成 label `annotation_argocd_argoproj_io_tracking_id`。

另外，**所有上述 series 必須帶有相同的 `cluster`、`namespace`，以及相同的
az / env label**（預設名稱 `az`、`env`，可替換，見第 7 節）。第二條以後的
query 都會用第一條 `kube_pod_owner` 讀到的 az / env 值做精確比對；若某個
controller annotation family 的 az / env label 與 `kube_pod_owner` 不一致，
該 family 在這裡會查不到東西（graph build 在未過濾時可能還能 adopt，這裡不會）。

---

## 4. 建立 `LabelQuerier`（`*promql.Router`）

### 4.1 單一 VictoriaMetrics endpoint

```go
table, err := promql.SingleBackendTable("http://vmselect:8481/select/0/prometheus", "", "")
if err != nil {
    return err
}
router, err := promql.NewRouter(table, nil, nil) // nil metrics、預設 client factory
if err != nil {
    return err
}
```

`SingleBackendTable` 產生一個名為 `default`、服務全部六個 family、不分 zone
的 backend。Basic auth 帶在第二、三個參數。

### 4.2 多 backend（依 zone 路由）

```go
table, err := backendsfile.Read("/etc/ksg/backends.yaml", nil) // nil = 從 process env 讀帳密
if err != nil {
    return err // 啟動時檔案無效應視為 fatal
}
router, err := promql.NewRouter(table, nil, nil)
if err != nil {
    return err
}
// 選用：熱重載
backendsfile.Start(ctx, router, backendsfile.ReloaderOptions{Path: "/etc/ksg/backends.yaml"}, 30*time.Second)
```

`backends.yaml` 至少要有 backend 服務 `ksm` family，例如：

```yaml
backends:
  - name: zone-a
    url: http://vm-a:8428
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-a]
  - name: zone-b
    url: http://vm-b:8428
    families: [ksm, kubelet, servicegraph, probe]
    zones: [zone-b]
```

若沒有任何 backend 服務 `ksm`，每次查詢都會回錯
`prom label query: family "ksm" is served by no backend`。
路由檔完整說明見 `docs/upstream-backend-routing.md`。

---

## 5. 呼叫方式

### 5.1 最小範例

```go
app, err := build.ResolvePodApplication(ctx, router, build.PodApplicationRequest{
    Cluster:   "c1",               // 原始 cluster label（不是 <az>-<env>-<cluster> 組合 identity）
    Namespace: "shop",
    Pod:       "checkout-7d9-abc",
    At:        time.Now().UTC(),   // 評估時間點（必填）
    Window:    5 * time.Minute,    // lookback window（必填、> 0）
})
switch {
case errors.Is(err, build.ErrAmbiguousPod):
    // 同名 pod 出現在多個 az 或 env；補上 AZ / Env 再查
case err != nil:
    // 請求欄位不合法，或任一上游 query 失敗
case app == "":
    // 查無 Application（無 controller owner、owner 種類沒有 annotation family、
    // 或 controller 沒有可用的 tracking-id）
default:
    // app 例如 "shop-app"
}
```

### 5.2 指定 zone / environment

```go
app, err := build.ResolvePodApplication(ctx, router, build.PodApplicationRequest{
    AZ:        "zone-a",   // 選填：路由到 zone-a 的 ksm backend，並加上 az matcher
    Env:       "prod",     // 選填：以 env="prod" 精確比對
    Cluster:   "c1",
    Namespace: "shop",
    Pod:       "checkout-7d9-abc",
    At:        end,
    Window:    5 * time.Minute,
})
```

建議：**呼叫端已知 zone 就一定要帶 `AZ`**。不帶時，第一條 `kube_pod_owner`
query 會 fan-out 到所有服務 `ksm` 的 backend。

---

## 6. `PodApplicationRequest` 欄位

| 欄位 | 必填 | 說明 |
|---|---|---|
| `Cluster` | ✅ | **原始** `cluster` label 值，等同 `?cluster=` 的語意；精確等式比對。 |
| `Namespace` | ✅ | pod 的 namespace。 |
| `Pod` | ✅ | pod 名稱。 |
| `At` | ✅ | 評估時間點；零值直接報錯（`pkg/promql` 不持有時鐘，不會預設成 now）。 |
| `Window` | ✅ | lookback window，須 > 0；渲染為 `last_over_time(...[<window>])`。 |
| `AZ` | ❌ | 可用區。有值時所有 query 都路由到該 zone 並帶 az matcher；留空時從 pod-owner series 讀出並釘住。 |
| `Env` | ❌ | 環境。有值時所有 query 帶 env 等式；留空時從 pod-owner series 讀出並釘住。 |
| `LabelKeys` | ❌ | 上游 az / env **label 名稱**，零值 = `az` / `env`。見第 8 節。 |

`Cluster` / `Namespace` / `Pod` 任一為空、`At` 為零、`Window <= 0`，都會在
**發出任何上游 query 之前**回錯，例如
`pod application lookup: Cluster is required`。

---

## 7. 解析流程與每一步發出的 query

### 7.1 流程

```text
(1) kube_pod_owner{pod, owner_is_controller="true"}
      │  0 筆 → 回傳 ""（查無 owner）
      │  讀出 az / env 並釘住（多組 → ErrAmbiguousPod）
      ▼
    取 controller owner：
      ReplicaSet → (2) kube_replicaset_owner{replicaset, owner_kind="Deployment"}
                   有 → 視為 Deployment；無 → 維持 ReplicaSet（裸 RS）
      多個 owner 時，先完成 RS→Deployment 折疊，再取字典序最小的 (kind, name)
      ▼
(3) kube_<kind>_annotations{<identity label>=<name>}
      取 annotation_argocd_argoproj_io_tracking_id；
      先剔除「解析後 Application 為空」的值（如 ":apps/..."），
      再取字典序最小的原始值；回傳第一個 ':' 之前的片段
      │  有 → 回傳
      │  無，且 kind 不是 Job → 回傳 ""
      ▼  （僅 Job）
(4) kube_job_owner{job_name, owner_kind="CronJob", owner_is_controller="true"}
      │  無 → 回傳 ""
      ▼
(5) kube_cronjob_annotations{cronjob=<name>}
      → 回傳 CronJob 的 Application（或 ""）
```

### 7.2 owner 種類 → annotation family 對照

| 解析後的 owner kind | Annotation series | 名稱 label |
|---|---|---|
| `Deployment` | `kube_deployment_annotations` | `deployment` |
| `StatefulSet` | `kube_statefulset_annotations` | `statefulset` |
| `DaemonSet` | `kube_daemonset_annotations` | `daemonset` |
| `ReplicaSet`（裸 RS） | `kube_replicaset_annotations` | `replicaset` |
| `Job` | `kube_job_annotations` | `job_name`（**不是** `job`） |
| `CronJob`（僅經 Job hop） | `kube_cronjob_annotations` | `cronjob` |

其他 kind（`ReplicationController`、`Node`、`Rollout` 等 CRD）沒有 annotation
family → 回傳 `""`、不報錯、**不發** annotation query。

### 7.3 各情境的 query 數量

| Pod 的 owner | 依序發出的 query | 數量 |
|---|---|---|
| StatefulSet / DaemonSet | pod_owner → statefulset/daemonset_annotations | 2 |
| Deployment（經 RS） | pod_owner → replicaset_owner → deployment_annotations | 3 |
| 裸 ReplicaSet | pod_owner → replicaset_owner → replicaset_annotations | 3 |
| Job，Job 本身有 tracking-id | pod_owner → job_annotations | 2 |
| Job，由 CronJob 管理 | pod_owner → job_annotations → job_owner → cronjob_annotations | 4 |
| 無 controller owner | pod_owner | 1 |
| 同名 pod 在多個 az/env | pod_owner（之後回 `ErrAmbiguousPod`） | 1 |

每一步依賴前一步結果，所以**必定循序**執行，不會並行。

---

## 8. Query 會帶哪些 label

### 8.1 渲染規則（`promql.LabelQuery.render`）

- 若 `LabelQuery.AZ` 非空（且 `ksm` family 會渲染 az matcher），**第一個**
  matcher 是 `<az-key>="<AZ>"`。
- 其餘 filter 依 **label 名稱字典序** 排列，全部是精確等式 `k="v"`（值經過
  escape）。
- `Window > 0` 時包成 `last_over_time(<selector>[<window>])`。
- 回傳的是 series 的 label set（去掉 `__name__`、去重、排序），**不回傳 sample 值**。

### 8.2 每條 query 帶的 label

所有 query 都固定帶：`cluster`、`namespace`，以及 az / env（規則見 8.3）。
各 query 額外帶：

| Query | 額外 label 等式 |
|---|---|
| `kube_pod_owner` | `pod=<Pod>`、`owner_is_controller="true"` |
| `kube_replicaset_owner` | `replicaset=<RS 名稱>`、`owner_kind="Deployment"` |
| `kube_<kind>_annotations` | `<名稱 label>=<controller 名稱>`（見 7.2） |
| `kube_job_owner` | `job_name=<Job 名稱>`、`owner_kind="CronJob"`、`owner_is_controller="true"` |

讀取的結果 label：`owner_kind`、`owner_name`、`annotation_argocd_argoproj_io_tracking_id`，
以及 az / env（僅第一條 query，用於釘住）。

### 8.3 az / env 的帶法

| 狀況 | `kube_pod_owner`（第一條） | 之後每一條 |
|---|---|---|
| 請求帶 `AZ="zone-a"` | 路由到 zone-a backend + `az="zone-a"` | 同左 |
| 請求沒帶 AZ，owner series 帶 `az="zone-a"` | 不帶 az、fan-out 到所有 ksm backend | 路由到 zone-a + `az="zone-a"` |
| 請求沒帶 AZ，owner series **沒有** az label | 同上 | 不路由（所有 ksm backend）+ filter `az=""`（PromQL 語意：label 不存在或為空） |
| 請求帶 `Env="prod"` | `env="prod"` | `env="prod"` |
| 請求沒帶 Env，owner series 帶 `env="prod"` | 不帶 env | `env="prod"` |
| 請求沒帶 Env，owner series 沒有 env label | 不帶 env | `env=""` |
| 請求沒帶 AZ/Env，owner series 出現多組 (az, env) | — | 不再發 query，回 `ErrAmbiguousPod` |

「釘住」的目的：避免另一個 zone / env 裡**同名**的 Deployment 或 CronJob
被讀進來。

### 8.4 實際渲染出的 PromQL（範例）

請求：`AZ="zone-a"`、`Env` 留空、`Cluster="c1"`、`Namespace="shop"`、
`Pod="web-abc"`、`Window=5m`；pod-owner series 帶 `env="prod"`，pod 由
ReplicaSet `web-7d9` → Deployment `web` 管理。

```promql
# (1)
last_over_time(kube_pod_owner{az="zone-a",cluster="c1",namespace="shop",owner_is_controller="true",pod="web-abc"}[5m])
# (2) env 已從 (1) 釘住
last_over_time(kube_replicaset_owner{az="zone-a",cluster="c1",env="prod",namespace="shop",owner_kind="Deployment",replicaset="web-7d9"}[5m])
# (3)
last_over_time(kube_deployment_annotations{az="zone-a",cluster="c1",deployment="web",env="prod",namespace="shop"}[5m])
```

CronJob 管理的 pod（Job `nightly-28000000` → CronJob `nightly`）後兩步：

```promql
last_over_time(kube_job_owner{az="zone-a",cluster="c1",env="prod",job_name="nightly-28000000",namespace="shop",owner_is_controller="true",owner_kind="CronJob"}[5m])
last_over_time(kube_cronjob_annotations{az="zone-a",cluster="c1",cronjob="nightly",env="prod",namespace="shop"}[5m])
```

> 與 graph build 的差異：這裡的 annotation query **不帶**
> `annotation_argocd_argoproj_io_tracking_id!=""` 這個固定 selector —
> 已經用 controller 名稱精確收斂到單一物件，空值在 Go 端剔除。

---

## 9. 怎麼替換 label

### 9.1 可以替換的：az / env 的 label 名稱

上游若不是用 `az` / `env` 當 label 名，透過 `PodApplicationRequest.LabelKeys`
指定，語意與 server 的 `--az-label` / `--env-label`（`KSG_AZ_LABEL` /
`KSG_ENV_LABEL`）相同：

```go
app, err := build.ResolvePodApplication(ctx, router, build.PodApplicationRequest{
    AZ:        "zone-a",
    Cluster:   "c1",
    Namespace: "shop",
    Pod:       "web-abc",
    At:        end,
    Window:    5 * time.Minute,
    LabelKeys: promql.LabelKeys{AZ: "zone", Env: "stage"},
})
```

渲染結果：

```promql
last_over_time(kube_pod_owner{zone="zone-a",cluster="c1",namespace="shop",owner_is_controller="true",pod="web-abc"}[5m])
last_over_time(kube_replicaset_owner{zone="zone-a",cluster="c1",namespace="shop",owner_kind="Deployment",replicaset="web-7d9",stage="prod"}[5m])
...
```

規則：

- 只影響 **label 名稱**；`AZ` / `Env` 欄位仍放**值**。
- 只設其中一個也可以，未設的那個用預設（`LabelKeys.OrDefault()`）。
- 名稱必須是合法 PromQL label 名（`^[a-zA-Z_][a-zA-Z0-9_]*$`），否則
  `QueryLabels` 回錯 `invalid az label key` / `invalid label filter key`。
- **AZ 與 Env 的 key 必須不同**。server 設定會擋，但這個 API 本身不檢查；
  若設成相同，env 的 filter 會覆蓋掉同名的 az filter，結果不可預期。
- 請與 server 用的設定保持一致（embedder 若也跑 graph build，把同一個
  `promql.LabelKeys` 傳給兩邊）。

### 9.2 不能替換的（寫死的 KSM 契約）

以下名稱是 kube-state-metrics 的固定 label 契約，**沒有設定可改**：

- `cluster`、`namespace`
- `pod`、`owner_kind`、`owner_name`、`owner_is_controller`
- `replicaset`、`deployment`、`statefulset`、`daemonset`、`job_name`、`cronjob`
- `annotation_argocd_argoproj_io_tracking_id`
- 所有 metric 名稱（沒有 metric prefix 設定；`--metric-prefix` 已移除）

上游若名稱不同，需要在 scrape / relabel 階段改成上述名稱。

---

## 10. 回傳值與錯誤

| 結果 | 意義 |
|---|---|
| `"<app>", nil` | 成功。值為 tracking-id 第一個 `:` 之前的片段（無 `:` 則原值）。 |
| `"", nil` | 查無 Application：無 controller owner、owner kind 沒有 annotation family、或 controller 無可用 tracking-id。 |
| `"", build.ErrAmbiguousPod` | 未帶 AZ/Env，且同一 `(cluster, namespace, pod)` 出現在多組 (az, env)。用 `errors.Is` 判斷，補 `AZ` / `Env` 重查。 |
| `"", 請求錯誤` | `Cluster/Namespace/Pod is required`、`evaluation instant is required`、`window must be positive`；未發出任何 query。 |
| `"", 上游錯誤` | 格式 `pod application lookup: <metric>: <原錯誤>`，例如某個 backend 失敗（fail-closed）、family 無 backend、結果超過 `promql.DefaultLabelQueryLimit`（10000）筆、值超過 1024 bytes 等。 |

tracking-id 挑選規則（與 build 相同）：

- 先剔除解析後為空的值（空字串、`:apps/...` 之類開頭就是 `:` 的值）。
- 剩下的取**原始字串字典序最小**者。
- 例：StatefulSet 同時有 `:apps/StatefulSet:shop/db` 與
  `db-app:apps/StatefulSet:shop/db` → 回傳 `db-app`。

Job 自己的 tracking-id 優先於 CronJob 的；Job 有自己的值時**不會**發出
`kube_job_owner` query。

---

## 11. 在你的程式裡寫測試

外部 module 可用 mockery 產生的 `MockLabelQuerier`，不必起假的 Prometheus
HTTP server：

```go
import (
    "testing"
    "time"

    "github.com/stretchr/testify/mock"
    "github.com/stretchr/testify/require"

    "github.com/akira-core/kube-state-graph/pkg/build"
    "github.com/akira-core/kube-state-graph/pkg/build/mocks"
    "github.com/akira-core/kube-state-graph/pkg/promql"
)

func TestLookup(t *testing.T) {
    q := mocks.NewMockLabelQuerier(t)
    q.EXPECT().QueryLabels(mock.Anything, mock.MatchedBy(func(r promql.LabelQuery) bool {
        return r.Metric == "kube_pod_owner"
    })).Return([]map[string]string{{
        "az": "zone-a", "env": "prod", "cluster": "c1", "namespace": "shop",
        "pod": "db-0", "owner_kind": "StatefulSet", "owner_name": "db", "owner_is_controller": "true",
    }}, nil)
    q.EXPECT().QueryLabels(mock.Anything, mock.MatchedBy(func(r promql.LabelQuery) bool {
        return r.Metric == "kube_statefulset_annotations" && r.Filters["statefulset"] == "db"
    })).Return([]map[string]string{{
        "statefulset": "db", "annotation_argocd_argoproj_io_tracking_id": "db-app:apps/StatefulSet:shop/db",
    }}, nil)

    app, err := build.ResolvePodApplication(t.Context(), q, build.PodApplicationRequest{
        AZ: "zone-a", Cluster: "c1", Namespace: "shop", Pod: "db-0",
        At: time.Unix(1_700_000_000, 0), Window: 5 * time.Minute,
    })
    require.NoError(t, err)
    require.Equal(t, "db-app", app)
}
```

`pkg/build` 自己的測試（`pkg/build/podapplication_test.go`）因 import cycle
改用 in-package 的 fixture store，而非這個 mock。

---

## 12. 限制與注意事項

- **一次一個 pod。** 要查大量 pod 請改用 graph build（`Builder.Build` /
  `kubegraph.Engine`），讀 `PodNode.Application()`。
- **只處理 pod。** Service / PVC 的 Application（以及 PVC 從掛載 pod 繼承）
  不在此 API 範圍。
- **沒有 HTTP route、沒有 `kubegraph` facade 方法。** 只能在 Go 程式內呼叫。
- **循序 round-trip。** 最多約 4 次上游請求；每次的延遲疊加，timeout 請用
  `ctx` 控制。
- **`Cluster` 是精確等式。** `Cluster="unknown"` 只匹配字面值 `unknown`，
  **不**包含缺少 `cluster` label 的 series（與 graph request 的 `unknown`
  bucket 不同）；沒有 `cluster` label 的 pod 無法用此 API 查詢。
- **不做 cluster identity adoption。** 第二條以後的 query 都比對釘住的
  az / env；controller family 的 az / env label 與 `kube_pod_owner` 不一致時
  會查無結果。
- **每條 query 各自讀一次路由表快照。** 查詢途中若 backends file 熱重載，
  不同步驟可能走到不同路由表；影響是暫時性的查無結果，不會得到錯誤的值。
- **不降級。** 與 build 不同，`kube_replicaset_annotations` /
  `kube_job_annotations` 查詢失敗會讓整個查詢失敗，而不是回空值 —— 避免把
  CronJob 的 Application 錯配給 Job。
