# 依 host/bind 選擇 RouteConfiguration（`pkg/route`）— 實作計畫

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 讓 `routeConfigNameFor` 在挑選 Envoy RouteConfiguration 時，除了 listener port 與 protocol，
也比對 request FQDN 與 `server.hosts`（Istio exact/wildcard 語意，與宣告順序無關），並讓 `server.bind`
正確反映在 RC name 上；同時新增 `RouteNoServerForHost` outcome，區分「port 猜錯」與「port 上的 listener
不服務此 host」。

**Architecture:** `pkg/route/gwresolve` 已具備 Istio 相容的 host 比對與 specificity 排序，故在該套件新增
`PickHosts`（server 層級的選擇入口）與 `SanitizeServerHost`（剝除 `<ns>/` 前綴），與既有的 `Resolve`
共用同一份 comparator。`pkg/route/translate` 的 `ScopedInput` 新增 `Host`，`routeConfigNameFor` 改為
host-aware，並以 `ListenerFor` 三態取代 `HasListenerOnPort` 的 `bool`，讓 `pkg/route/resolver.go` 能回報
新的 `build.RouteNoServerForHost`。

**Tech Stack:** Go 1.26、istio.io/istio（in-process istiod translation）、envoy go-control-plane、testify、
testcontainers-go（integration）。

**Background reading before starting:**
- `pkg/route/translate/translate.go` — `routeConfigNameFor` (L160-182)、`HasListenerOnPort` (L135-142)、
  `Translate` (L91-127)。
- `pkg/route/gwresolve/gwresolve.go` — `specificity` / `New` / `Resolve` / `ResolveAmong`。
- `pkg/route/resolver.go` — `resolveConfig` (L223-254)、`outcomeRank` (L62-71)、segment cache (L128-131)。
- `pkg/build/routeresolve.go` — `RouteOutcome` 常數 (L63-95)；`pkg/build/routeprescan.go` — `route_engine_*`
  reason 字串。
- 上游 ground truth：`pilot/pkg/model/gateway.go` 的 `gatewayRDSRouteName`（未匯出）與
  `pilot/pkg/networking/core/gateway.go` 的 `buildGatewayHTTPRouteConfig`。
- CLAUDE.md 的 D5 段落；`openspec/changes/translate-global-fqdn-to-k8s-service/design.md` D5。

**Conventions:**
- TDD：先把測試改成新預期（RED），跑一次確認失敗，再改實作（GREEN）。
- 每個 task 完成後 commit。
- 本變更**不動** node/edge/attribute/`labels`，golden 快照不受影響。
- `pkg/build` 不得 import `pkg/route`（design D1）；`make check-route-containment` 會檢查。

---

## 背景：現況為何是錯的

[translate.go:160-182](../../../pkg/route/translate/translate.go) 的 `routeConfigNameFor` 只用
**listener port + protocol** 挑 RC：遇到第一個 port 相符的 server 就 return，既不比對 request FQDN 與
`server.hosts`，也不處理 `server.bind`。resolver 只在 Gateway 層級依 host 選擇
([resolver.go:147](../../../pkg/route/resolver.go))，Gateway 內部沒有任何一層再選 server。

Istio 真正的命名規則（`gatewayRDSRouteName`）是：

```go
func gatewayRDSRouteName(server *networking.Server, portNumber uint32, cfg config.Config) string {
	p := protocol.Parse(server.Port.Protocol)
	bind := ""
	if server.Bind != "" {
		bind = "." + server.Bind
	}
	if p.IsHTTP() {
		return "http" + "." + strconv.Itoa(int(portNumber)) + bind
	}
	if p == protocol.HTTPS && !gateway.IsPassThroughServer(server) {
		return "https" + "." + strconv.Itoa(int(server.Port.Number)) + "." +
			server.Port.Name + "." + cfg.Name + "." + cfg.Namespace + bind
	}
	return ""
}
```

即：每個 TLS terminate 的 HTTPS server **各自**擁有一個 RC（每張憑證 / SNI 一條 filter chain）；純 HTTP
servers 則**共用** `http.<port>[.<bind>]`，host 的區分發生在 RC 內部的 virtual hosts。因此現況：

- **HTTPS、同一 port 多個 servers** → 一定選錯 RC。多半不是解析成錯的 Service：拿 `admin.example.com`
  去查 `api` 的 RC 匹配不到 VirtualHost → `RouteNoRoute` → external node，也就是**靜默的 false negative**。
- **HTTP 帶 bind** → 選錯 RC，且算出的 name 與 istiod 實際發出的不一致。
- 純 HTTP、無 bind、單一 listener → 不受影響（共用 RC）。

## 已定案的決策

1. **新增 outcome `RouteNoServerForHost`**（reason `route_engine_no_server_for_host`），rank 排在
   `RouteNoListenerOnPort` 與 `RouteNoRoute` 之間，讓 design-D5「猜錯 port」的訊號不被混淆。代價是
   `HasListenerOnPort` 的 `bool` 必須改成三態。
2. **兩層都修 `<ns>/` host prefix**。Istio 允許 `prod/*.example.com`、`./host`、`*/host`；直接餵給
   `host.Name.Matches` 會靜默匹配不到任何東西。先例是 `model.GetSNIHostsForServer`：比對前無條件剝掉
   `<ns>/`。namespace 前綴限制的是**哪些 VirtualService 能綁定**，從不限制 client host —— 所以是**剝除，
   不是過濾**。（已由上游佐證：VS 綁定走 `host.NamesForNamespace`（過濾），client/SNI 比對走
   `GetSNIHostsForServer`（剝除），兩者是不同問題。）
3. **host 匹配不到就直接結束，不做 HTTP fallback**。理由見下。

### 為什麼「沒有 host 匹配」可以直接結束 —— 已對上游驗證

`buildGatewayHTTPRouteConfig`（`pilot/pkg/networking/core/gateway.go:424-431`）建 vhost 時用的是：

```go
serverHosts := host.NamesForNamespace(server.Hosts, virtualService.Namespace)
intersectingHosts := serverHosts.Intersection(virtualServiceHosts)
```

而 `Names.Intersection`（`pkg/config/host/names.go:83-99`）以 `SubsetOf` 判斷，**永遠保留兩者中較具體的
那一個**。所以每個 vhost domain `d` 必定是某個 server host pattern 的子集；若 reqHost 匹配 `d`，則 reqHost
也必然匹配該 server host。

**逆否命題即結論**：若 reqHost 匹配不到該 port 上的**任何** server host，就不可能匹配到任何 vhost —— 結果
必為 miss。回傳 `http.<port>` 再跑一次完整的 istiod translate ＋ `router_check_tool` subprocess（約
50–60ms）只是為了抵達一個已註定的 `RouteNoRoute`。直接回報 `ListenerNoServerForHost` 因此是**等價但更快、
診斷更精確**的，而且讓規則少掉 bind / `IsHTTP()` 兩個例外分支。

（`httpsRedirect` 產生的 vhost 直接來自 `server.Hosts`，同樣被 server hosts 界定；`vHostDedupMap` 為空時
istio 補一個 default 404 vhost —— 兩者都不會推翻上述結論。）

**對既有行為的影響**：純 HTTP 單一 listener 且 host 匹配得到時，name 仍是 `http.<port>`，完全不變。只有
「host 匹配不到」時，reason 從 `route_engine_miss`（`RouteNoRoute`）變成 `route_engine_no_server_for_host`。
兩者都退回 external node，**圖的輸出不變**，只有診斷字串更精確。

---

## Task 1 — gwresolve：一個 sanitizer、一個 comparator、兩個入口

- [x] 1.1 `pkg/route/gwresolve/gwresolve.go`：在 `pat` 加 `idx int` 欄位（`PickHosts` 用；`New` 恆為 0）。
- [x] 1.2 新增 `SanitizeServerHost(h string) string` —— `strings.Cut(h, "/")`，有 `/` 就取後段。註解要說明
      與 `host.NamesForNamespace`（過濾）的不對稱：那是「VS 能否綁定」，這是「server 是否服務此 client host」。
- [x] 1.3 把 `New` 的 pattern 建置/排序抽成 `newPats(key string, hosts []string) []pat` 與 `sortPats(pats []pat)`；
      `newPats` 內對每個 host 套用 `SanitizeServerHost`（故 `New` 一併修好 `<ns>/` 前綴）。
      `sortPats` 排序鍵：score desc → pattern asc → **idx asc**（相同 pattern 取較小索引，對齊 istio
      `CheckDuplicates`「先宣告者勝」；`New` 下 idx 全為 0，該分支為 no-op）。
- [x] 1.4 新增 `PickHosts(hostSets [][]string, reqHost string) (int, bool)`：對每個集合以 `newPats` 產生
      pattern 並標上 `idx`，`sortPats` 後回傳第一個匹配的 `idx`。**索引不可進入 comparator 當字串** ——
      否則 `"10" < "2"`。
- [x] 1.5 測試（`gwresolve_test.go`，internal，純 `testing`）：
      `TestSanitizeServerHost`（`prod/*.example.com`、`./host`、`*/host`、無前綴、`*`）；
      `TestPickHosts`（exact 勝 wildcard 且兩種宣告順序各驗一次、較具體 wildcard 勝、`*` 最不具體、
      無匹配、空 host set、無 sets、`ns/` 前綴、多 host 集合取最佳 pattern、**相同 pattern → 較小索引**）；
      `TestPickHostsIndexNotLexicographic`（12 個集合，證明索引不會造成字典序錯位）；
      `TestResolveMostSpecific` 補一個 `{Name: "gw-scoped", Hosts: []string{"prod/*.scoped.example.com"}}`
      當作 1.2 的回歸釘。
- [x] 1.6 `go test ./pkg/route/gwresolve/ -count=1` 綠燈後 commit。

## Task 2 — translate：host-aware 選擇

- [x] 2.1 `ScopedInput` 新增 `Host string`（`""` ⇒ host-agnostic）。註解說明：它與既有的 `Port` 是同一類、
      經同一條路徑抵達的 request-derived 純量；解析真實請求的呼叫端 **MUST** 設定它。
- [x] 2.2 新增 `ListenerStatus` 三態與 `ListenerFor(in ScopedInput) (string, ListenerStatus)`：
      `ListenerFound` / `ListenerNoneOnPort`（該 port 無 server，或選中的 server 無 HTTP RDS route）/
      `ListenerNoServerForHost`（該 port 有 server，但無一服務此 host）。`ListenerFor` 是**唯一**的
      RC 選擇決策點，`Translate` 與 resolver 都走它，兩者不可能分歧。
- [x] 2.3 新增 `rdsRouteName(s *networking.Server, port int, gwCfg config.Config) string` —— `gatewayRDSRouteName`
      的逐行移植，**含 bind**。註解要記兩件事：(a) HTTPS 用 `s.Port.Number`、HTTP 用 resolved port 的
      不對稱是上游的，此處兩者一致是因為呼叫端已用 `s.Port.Number == port` 過濾；(b) **不要**換成
      `gateway.IsHTTPSServerWithTLSTermination` —— 它多了 `Tls != nil` guard，而 `IsPassThroughServer`
      在 `Tls == nil` 時回 false，對 `tls: nil` 的 HTTPS server 兩者結論相反；我們鏡射的是 RC **name**，
      不是 filter-chain 分支。
- [x] 2.4 改寫 `routeConfigNameFor(gwCfg, port, reqHost) (string, ListenerStatus)`：
      1. `port <= 0` → 80；`onPort` = `Port != nil && Port.Number == port` 的 servers。
      2. `len(onPort) == 0` → `ListenerNoneOnPort`。
      3. `reqHost == ""` → `rdsRouteName(onPort[0], …)`（host-agnostic 逃生口）。
      4. `PickHosts(onPort 的 hosts, reqHost)` 匹配不到 → **`ListenerNoServerForHost`，結束**。
      5. 匹配到 → `rdsRouteName(onPort[idx], …)`；name 為 `""`（passthrough/TCP 贏得該 SNI）→
         `ListenerNoneOnPort`；否則 `ListenerFound`。
      **`onPort` 不可先依 protocol 過濾** —— 若 passthrough server 在 host 上匹配得更具體，istio 會把
      filter chain 給它，它必須先贏得選擇、再回報 miss；先濾掉等於憑空發明一條 Envoy 永遠不會走的 route。
- [x] 2.5 移除 `HasListenerOnPort`；`Translate` 改用 `name, st := ListenerFor(in)`，`st != ListenerFound`
      維持今天的空 RC 回傳。更新 package doc（L9-12 目前把 port-only 規則寫成契約）。
- [x] 2.6 測試（`translate_test.go`，external package）：新增 `tlsServer` / `passthroughServer` /
      `httpServer` / `gwWithServers` / `reversed` fixtures（注意 `ServerTLSSettings_TLSmode` 的零值是
      **PASSTHROUGH**，HTTP server 的 `Tls` 必須為 nil）。
      `TestListenerForSelectsServerByHost`：驗收組合（`admin.example.com` → `https.443.admin.gw-000.istio-system`；
      `api.example.com` → `https.443.api...`）、exact 勝 wildcard、較具體 wildcard 勝、HTTP bind
      （`http.80.10.0.0.2`）、HTTPS bind（`https.443.api.gw-000.istio-system.10.0.0.1`）、`ns/` 前綴、
      host 無匹配（HTTPS-only 與 HTTP-only 各一）→ `ListenerNoServerForHost`、HTTP 共用 `http.80`、
      passthrough 贏得 SNI → `ListenerNoneOnPort`、port 無 listener、`*` catch-all。
      **每個 case 都要以正反兩種宣告順序各跑一次**（order-independence 是本次的核心性質）。
      `TestTranslateHTTPSServerByHostEndToEnd`：兩個 :443 TLS server ＋ 各自的 VS，經 istiod 實際翻譯，
      斷言 RC name **與**該 host 自己的後端 cluster —— 這是 port-only 選擇下會靜默 miss 的案例。
      `TestTranslatePortSelectsRouteConfig` 的 `hasListener bool` 改為 `wantStatus translate.ListenerStatus`，
      `Host` 留空以證明 host-agnostic 路徑逐位元組不變。
- [x] 2.7 `go test ./pkg/route/translate/ -count=1` 綠燈後 commit。
      注意：首次編譯 istio 相依很慢（數分鐘），請預留時間或先暖 build cache。

## Task 3 — build：新 outcome 與 prescan reason

- [x] 3.1 `pkg/build/routeresolve.go`：新增 `RouteNoServerForHost RouteOutcome = "no_server_for_host"`
      與註解；一併修訂 `RouteNoListenerOnPort` 的註解，讓兩者讀起來明確互斥。
- [x] 3.2 `pkg/build/routeprescan.go`：在 `default` catch-all 前加 `case entry.outcome == RouteNoServerForHost`，
      以與 `no_listener_on_port` 相同的 key 集合發出 `route_engine_no_server_for_host`。
- [x] 3.3 `pkg/build/routeprescan_test.go` 的 outcome 表補上新 case；`go test ./pkg/build/ -count=1` 後 commit。

## Task 4 — resolver：接上新的 listener gate

- [x] 4.1 `resolveConfig`：在 `scoped.Port = req.Port` 旁加 `scoped.Host = req.Host`，並附註解說明 host
      決定「port 上哪個 server 擁有此 RC」。
- [x] 4.2 把 `if !translate.HasListenerOnPort(...)` 換成 `switch _, st := translate.ListenerFor(scoped); st`，
      分別回傳 `RouteNoListenerOnPort` / `RouteNoServerForHost`。註解點出兩個 gate 都只看 config，
      不花 translate round-trip 也不 exec `router_check_tool`。
- [x] 4.3 `outcomeRank`：`RouteNoRoute` 3、`RouteNoServerForHost` 2、`RouteNoListenerOnPort` 1、default 0，
      並更新註解。
- [x] 4.4 segment cache 註解（L128-131）：「Port and path are constant within a request」→ 加入 host，
      否則下一位讀者不會知道這個不變量現在涵蓋三個值。
- [x] 4.5 測試：新增 `TestResolveRoute_HostNotServedByAnyServerOnPort`。`resolveConfig` 在 listener gate
      就回傳，**早於** `Translate` 也早於碰到 `r.run`，故可用 zero `matchcheck.Runner` 單元測試（既有測試
      已仰賴此性質）。window 內一列 `GatewayRow`，`ServerHosts: ["*.example.com"]`（使 Gateway 層匹配成功），
      `SpecJSON` 帶兩個 `:443` HTTPS server（`api`/`admin`），以 `protojson.Marshal` 序列化 typed
      `networking.Gateway`（比照 `memwindow_test.go`）；request `(443, other.example.com)` → 斷言
      `RouteNoServerForHost`。這一測釘死 `Host` 的傳遞：若未設 `scoped.Host`，請求會走進 RC 路徑並在
      zero Runner 上炸開。
- [x] 4.6 `go test ./pkg/route/... -count=1 -race` 綠燈後 commit。

## Task 5 — 文件與 openspec

- [x] 5.1 `CLAUDE.md`：D5 段落的 reason 清單加入 `route_engine_no_server_for_host`，並修正「`routeConfigNameFor`
      找 port 相符的 server」的描述為 host-aware。
- [x] 5.2 `openspec/changes/translate-global-fqdn-to-k8s-service/design.md`：改寫 D5（目前明文寫著 port-only
      規則），補上 host 選擇、bind、以及「沒有 host 匹配可直接結束」的 `Intersection` 論證。
- [x] 5.3 同一 change 的 `tasks.md` 新增 **第 14 節**（該 change 仍在進行中，tasks 已到 13.9，故歸屬於此
      而非另開 change）。
- [x] 5.4 `specs/pod-service-graph/spec.md`：在既有的「No listener on the derived port」場景旁，補一個
      依 host 選 server 的場景。
- [x] 5.5 `openspec validate translate-global-fqdn-to-k8s-service` 後 commit。

## Task 6 — 全面驗證

- [x] 6.1 `go build ./... && go test ./pkg/route/... ./pkg/build/... -count=1 -race`
- [x] 6.2 `make test && make lint && make check-route-containment`
- [x] 6.3 `KSG_ROUTER_CHECK_BIN=<path> go test ./internal/integration/ -run TestRouteSuite`（需
      `router_check_tool`）
- [x] 6.4 `go test ./pkg/route/ -tags oracle` —— **最關鍵的一項**：它產生 gw/vs 語料庫並以
      gwresolve→translate→`router_check_tool` 交叉驗證，是「新 name 是否與 istiod 實際發出的一致」最強的檢查。
- [x] 6.5 golden 測試應完全不受影響（無 node/edge/attribute 變更）—— 若有 diff，代表某處改動溢出了預期範圍。

---

## 附註

- `translate → gwresolve` 的 import 方向不產生循環（gwresolve 只 import `sort`、`strings` ＋
  `istio.io/istio/pkg/config/host`），也沒有新依賴 —— `pkg/route` 本來就連結 istio，
  `make check-route-containment` 維持綠燈。
- 已知的相鄰缺陷，**不在本次範圍**：`candsToGateways` / `candNames`
  （[resolver.go:283-297](../../../pkg/route/resolver.go)）只用 `c.Name`、丟掉 `c.Namespace`，
  而 `candidatesAt` 的 dedup key 是 `ns/name` —— 兩個 namespace 同名的 gateway 會在 gwresolve 的
  allow-set 撞在一起。另外上游 `resolvePorts` 會做 Service port → TargetPort 轉換（可能一個 server
  產生多個 name），本計畫維持以 `s.Port.Number == port` 直接比對 request port。
