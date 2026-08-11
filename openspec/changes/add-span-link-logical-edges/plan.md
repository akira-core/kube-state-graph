# Span-link 邏輯邊 + transport 邊虛線標記(edge `labels.relation`)

## Context

前端要把「邏輯相依」(producer pod 經 nats/mongo broker 被 consumer pod 消費,由跨 trace-ID 的 span link 產生)畫實線、把「網路相依」(pod 到 broker 的實際連線)畫虛線。上游 `traces_service_graph_request_total` 新增 `edge_relation="link"` 系列:雙端 pod UID + **兩側各自**的 broker peer-address label(client 側沿用既有 `client_server_address`/`client_net_peer_name`/`client_dns_answers`/`client_server_port`;server 側新增鏡像 `server_server_address`/`server_net_peer_name`/`server_dns_answers`/`server_server_port`,無 "via" 字串、v1 無 `server_network_peer_address`)。

ksg 端:link 系列產出 `relation=link` 的邏輯邊;兩側 peer label 以**與既有 unknown-server enrichment 完全同一條分類鏈**(lookup-only)解析出 broker node ID,把對應的 pod→broker 邊標 `relation=transport`。使用者已選定 **edge 標記**(非 node 標記)。效能與 memory 紀律是硬需求。

## 核心設計決策

- **標記 = 建邊時的集合成員查表**:`linkPairs` / `transportPairs` 兩個 `map[pairKey]struct{}`,與現有 `pairs` 同為 `parseWithResolver` 的 **function-local**(servicegraph.go:338-343 旁)— 無 resolver 欄位膨脹、無全域、無跨 build 狀態(隨函式結束回收,零 leak 面)。
- **link 贏 transport、link 贏一般系列**:建邊時先查 `linkPairs` 再查 `transportPairs`;set 累加 monotone ⇒ 樣本順序無關(D6)。
- **via 解析 lookup-only**:絕不 materialise(孤兒防護,`resolveRouteChain` 先例 servicegraph.go:1146-1149)。network 邊缺席時 via pair 純 marker、彙總 debug log、零合成。
- **昂貴路徑(route engine)靠既有 prescan `seen` map 去重**:link via key 與同 broker 的一般系列 key 由同一 `peerRouteKey` 推導 ⇒ 同構合一,一次 store read — 消滅「同 broker 被解析 4 次」。in-memory 分類不 memoise(servicegraph.go:934-938 明文先例)。
- **D30 selector 不變**;`edge_relation` 只是 parse 新讀一個 label。UUIDv5 edge ID 不含 labels ⇒ ID 全部不變。
- **`service-selects-pod` fan-out 邊永不標記**(共享邊)。

## 修改檔案

### 1. `pkg/graph/registry.go`(:63-65, :74-76)
`pod-calls-pod`、`pod-calls-service` 的 `Labels` 各加 `{Name:"relation", ValueType:"string", Description:"..."}`(值域 `link`/`transport`,一般邊 absent)。`service-selects-pod` 不加。→ `/v1/edge-types` + `edge-types.json` golden regen。cytoscape 序列化原樣透傳 edge labels,無變更。

### 2. `pkg/build/routeprescan.go`
- **`serverPeerLabelsOf(m model.Metric) peerLabels`**:重用既有 `peerLabels` struct(:32-39),讀 4 個 `server_*` label,其餘欄位留空(`value()`/`derivePort` 跳過空欄位 ⇒ D1/D5 precedence 原樣適用)。單 struct 兩建構子,零分叉。
- **`viaRouteKey(ownPod *graph.PodNode, peer peerLabels) (routeKey, bool)`**:把現行 :299-322 的 skip 鏈(`peerRouteKey` → classified+`anchorHolds` skip → `lookupPeerPodIP` hit skip → `ips==""` skip)抽成共用方法;既有 unknown-server 分支改呼叫它(行為 byte-for-byte,既有測試守恆)。
- **`collectRouteQueriesWith`(:262)link 分支**:`edge_relation=="link"` 時,client pod(`lookupClientPod`)與 server pod(`podByUID`)各自解析成功才發 via key(anchor = 各自 pod 的 cluster),經 `seen` 去重;之後 **fall through** 既有 `server=="unknown"` 分支(server unknown 的 link 系列其 client-via key 與該分支 key 同構,`seen` 合一)。共同前置(cluster bucket、`normalizeSelfLoopUIDs`、wholly-empty skip)重排至分支前。

### 3. `pkg/build/servicegraph.go`
- 常數:`relationLabelKey="relation"`, `relationLink="link"`, `relationTransport="transport"`(:24 旁)。
- **`viaNodeID(ownPod *graph.PodNode, peer peerLabels) (string, bool)`**:`resolveUnknownServerPeer`(:822)分類鏈的 lookup-only 鏡像 — `value()` 空→false;`truncateBracketSuffix`+`splitPeerAddressPort`;`classifyPeerHost`(:658)+`anchorHolds`(:722)→`graph.ServiceID`;IP→`lookupPeerPodIP`(:701)→pod ID;route 索引→`routeNodeID`;fallback `graph.ExternalID(raw)`。抗漂移:全走既有純 helper。
- **`routeNodeID(key routeKey, raw string) string`**:`routeIndexResolve`(routeprescan.go:447)的 lookup-only 變體 — `RouteHit`/`RouteIngressLBService` 且 dest cluster 持有 service → `ServiceID(dest...)`(RouteHit 只取 backend,不含 ingress hop);其餘一律 `ExternalID(raw)`。不 materialise、不標 role、不觸發 chain。
- **`parseWithResolver`(:324)**:每樣本讀 `edge_relation`;`isLink` 時在 cross product 後:
  - pair 分類:`serverLabel=="unknown" && (serverUID=="" || podByUID miss)` ⇒ 該 pair 走了 `resolveUnknownServerPeer`(client peer = broker 位址),產出是 A→broker **transport**,入 `transportPairs`;其餘(真 pod / synth pod / D27 ghost external)入 `linkPairs`。
  - 逐側 via:`clientPod != nil` → `viaNodeID(clientPod, peer)` 入 `transportPairs`;server pod 解析到 → `viaNodeID(serverPod, serverPeerLabelsOf(s.Metric))` 入 `transportPairs`。任一側未解析 = 該側不標,另一側照常(per-side 獨立)。
  - Degrade 全部 fall-through 既有路徑(D27 / synth pod / wholly-empty drop),只加兩個彙總 debug:server-unknown link 改標 transport、transport pair 無對應邊(marker-only)。
- **建邊迴圈(:457-475)**:`linkPairs` 命中 → `labels["relation"]="link"`;else `transportPairs` 命中 → `"transport"`。`svcEdges`(:476)與 `routeChainEdges`(:485)不查表。迴圈後統計 unmatched transport pairs(排除已是 link 的),`slog.Debug` 一行彙總。

## 測試

- **`pkg/build/servicegraph_test.go`** `TestParseServiceGraph_LinkRelation_*`(用 `sampleTopologyWithServices`/`sampleVec`):BothPodsResolve 帶 link;LinkWinsOverPlainSeries 雙順序(D6);TransportMarksExistingBrokerEdge;LinkBeatsTransportOnCollision;ServerUnknown_PairMarkedTransport(與 network 邊合併單邊);ServerGhostExternal_KeepsLink;ServerSynthPod_KeepsLink;ClientUnresolved_NoClientVia_ServerViaStillMarks;**ViaLookupNeverMaterialises**(只有 link 系列 ⇒ 零 service/external node、零 fan-out);FanOutNeverMarked;SelfLoopLinkSeries(D33 不觸發);UnmatchedTransportPair_MarkerOnly;NonLinkRelationValueIgnored。route 路徑用手工 `routeIndex` 餵 `parseServiceGraphRoutes` 驗 `routeNodeID` 與 materialise 路徑 ID 對齊。
- **`pkg/build/routeprescan_test.go`**:LinkSeries_EmitsBothViaKeys(server key 的 callerCluster = server pod 自己的 cluster);DedupsWithPlainNetworkSeriesKey;viaRouteKey skip 鏈各 case;ServerUnknownLinkSeries_SingleKey;`SameFQDN_MultipleSeries_SingleRouteKey`(同一 anchor cluster 內多條系列 — 多個 client pod、一般 unknown-server 系列 + link 系列混合 — 攜同一 broker FQDN ⇒ `collectRouteQueriesWith` 只產出**一個** routeKey,prescan `seen` map 合一);`TestReadServiceGraph_SameFQDNResolvedOnce`(端到端 cache 斷言:`ReadServiceGraph` 配既有 `fakeRouteResolver`,餵 N 條同 FQDN 系列(link + 一般混合)⇒ `requests()` **恰一筆** — 同一次 query service graph 內同 FQDN 只打一次 route store,其餘系列全部由 prefetched `routeIndex` 查表命中,且所有對應邊仍解析到同一目的節點)。對照註記:不同 cluster 的同 FQDN 仍各自成 key(routeKey 含 `callerCluster`,設計如此,非重複解析);in-memory 分類鏈依明文先例(servicegraph.go:934-938)**不** memoise,cache 斷言只針對昂貴的 route-engine store read。既有測試全綠。
- **Golden**:`edge-types.json` regen;新 fixture `link-relation-cytoscape.json`(A→B link、A→broker transport、fan-out 無 relation)。
- **Integration** `internal/integration/graph_e2e_test.go` `TestSpanLinkRelationEdges`:two-sample 摻入模式(0@t-60s、60@t),link 系列 + unknown-server network 系列 + `kube_service_info`/endpointslice topology ⇒ 斷言三種邊的 relation 標記。

## OpenSpec(先於程式碼)

`openspec/changes/add-span-link-logical-edges/`:proposal / design(決策:set-only monotone link>transport;via lookup-only;server 側鏡像重用 peerLabels;prescan seen 去重;server=="unknown" link 判 transport;marker-only 不合成)/ specs delta(`pod-service-graph`:ADDED「Span-link logical edge relation marking」含各 degrade scenario;`graph-api`:MODIFIED edge-types labels 陣列)/ tasks。完工後 CLAUDE.md 補一條 load-bearing bullet。

## 實作順序

1. OpenSpec change + `openspec validate`
2. registry + `edge-types.json` regen
3. routeprescan:`serverPeerLabelsOf` → `viaRouteKey` 重構(既有測試不變)→ link 分支 + 測試
4. servicegraph:`viaNodeID`/`routeNodeID` → parse link 處理 → 建邊標記 → 單元測試
5. Golden fixture + integration 測試
6. `make test`(含 -race -shuffle=on)、`make lint`、`make docs` 若 swagger 受影響

## 風險 / 邊角

- **maxRouteKeys=512 cap**:link 系列每條最多 +2 key;被截斷的 key 在索引無 entry ⇒ `routeNodeID` 與 materialise 路徑共用同一索引,一致退化 `ExternalID(raw)`,標記不錯位(既有 Warn log 照報)。
- **via/materialise 對齊**:唯一刻意分歧 = RouteHit 只取 backend、不標 role、不觸發 chain — design.md 明文。
- **cardinality**:`edge_relation` 增加系列數,`pairs` 聚合天然去重。
- **決定性**:標記純集合函式、log 只彙總計數、`SortEdges` 不變 ⇒ golden 可重現。

## 驗證

```bash
make test && make lint
go test ./pkg/build/ -run TestParseServiceGraph_LinkRelation -v
go test ./internal/api/ -update -run Golden   # 刻意變更後 regen
go test ./internal/integration/ -run TestSpanLinkRelationEdges -v   # 需 Docker
openspec verify add-span-link-logical-edges
```
