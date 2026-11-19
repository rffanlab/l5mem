# 设计与实现对照

设计来源：《第五层记忆（L5）设计逻辑》v2.6（牛马一号汇总）。这里按章节说明 Go 版怎么落地、哪里有改动。

## 存储（设计第 2 节）

- `entries` 一行一条：内容组（kind/topic/body/tags/source）、时间组（created_at 人读 + created_epoch 机读）、
  生命周期组（valid_to、superseded_by、volatility、recheck_epoch、last_verified_*）、身份组（content_hash 唯一、meta JSON）。
- `content_hash = sha256(kind | 规整后的 topic | 规整后的 body)`。并发写同一条：第二个返回「已存在 #id」，不抛错；带取代关系的重放也安全。
- 旧事实不删：`--supersedes` / `supersede` 设 superseded_by + valid_to；`retract` 用于「没有新值、只是错了」。`history` 沿取代链前后追。
- 索引与本体分离：`fts_bi`（二元组）、`fts_tri`（三元组）、`entry_tags`（标签倒排）全部可由 `repair` 从 entries 重建；`doctor` 核对行数、孤儿、标签一致性、FTS 完整性。

## 检索（第 3 节）

- 四路：bigram（中文重叠二元组 + 西文词前缀，unicode61）、trigram（≥3 字子串）、topic（`topic >= kw AND topic < kw||U+10FFFF` 的 BINARY 范围查）、
  tag（点查，扇出上限 `max(60, 12n)`，超限放弃该标签并在结果里报告）。单个汉字走有界 LIKE（只扫最近 2 万条）。
- RRF（k=60）融合；`lex` 归一到 0～1（除以候选中的最大 RRF）。
- 打分 `0.45·lex + 0.15·proximity + 0.25·recency + 0.15·importance (+ 标签加分)`；proximity 只对第一轮前 3n 条算。
- 读路径零写库：访问次数记在内存，每 5 分钟 / consolidate / 退出时批量回写。进程内 LRU 512，任何写入整体失效（单进程单写者，不会读到旧数据）。
- `--explain` 输出每条的四个分量和各路名次。

**改动**
1. 新增 kind `judgment`（判断，基线 0.85）：InfoCollector 每期文章「我们的思考」是判断，不是决策也不是事实。
2. `L5_STABLE_FLAT=1`（默认）：stable 记忆的新鲜度恒为 1。原则、教训不该因为是两年前写的就排在昨天的备忘后面。设 0 恢复原设计。
3. `L5_TAG_BOOST`（默认 0.10）：实现「查询词=标签名时顶到第一梯队」。原设计只靠 RRF，标签路和其他路平权时，标签命中不一定能到第一。
4. 高频词的 bm25 窗口、detail=full、元数据常驻内存：见 BENCH.md。

## 联想（3.3）

`relate <id>`：锚点的标签 → 共享标签的其他条目（第一跳，按 IDF 加权）；从得分最高的 5 个邻居出发走锚点没有的标签（第二跳，权重减半）。
只出现一次的标签 IDF=0，超过扇出上限 4 倍的枢纽标签直接跳过。

## 生命周期（第 4 节）

- stable / slow（180 天）/ volatile（30 天）；`--unverified` 未核实缩到 7 天。三种时间分开：valid_to（被取代）≠ recheck_epoch（变陈旧）≠ 新鲜度。
- 自动分级按可信度取序：主题快变词 > 主题稳定词 > kind 稳定类 > 正文快变词 > 正文稳定词 > 默认 slow；`--vol` 显式覆盖后不再被 `reclassify` 改。
- 每日巡检（`L5_WATCH_AT`，默认 10:17，进程内定时，不靠 cron）：consolidate + 逾期 ≥7 天升级 + 体检 + 使用日报增量；没有发现就 silent，有发现才 POST 到 `L5_NOTIFY_URL`。
- 每周的「联网核对」由调用方做（Hermes 的 research-watch 照旧）；InfoCollector 每周一 10:37 用自己收集的报道核对到期的快变事实：
  仍成立 → verify（备注佐证的事件）；变了 → 新值取代旧值（只自动取代 InfoCollector 自己写的，别人写的只 verify）。

## 观测（第 5 节）

- `usage.jsonl`：search/add/relate/due/verify/get/history/supersede/retract 每次一行（caller、关键词、耗时、命中数、tag_top1、tag_cands、cached）；
  维护命令和 caller=watchdog 不记。异步写，队列满了丢，日志不能拖慢检索。
- `metrics [--since-last]`：谁在用、各命令 p50/p95、标签命中率、标签路 vs 非标签路延迟、联想次数、空手而归率、规模；
  预设判决线（标签命中率、标签路 2 倍红线、联想两周零使用、空结果过半）写在报告里。

## 迁移（第 6 节）

- `export` → JSONL（保留 id、取代链、核对记录）并输出指纹；`import` 保留 id 写入，三重断言：行数 → 指纹（id/kind/topic/body/tags/source/created_at 拼接 SHA256）→ 回读自检（主题限定 + 正文开头检索回自己；超过 2000 条均匀抽样）。
- `import-sqlite 旧库.db`：只读打开别的 L5 库（例如 Hermes 机器人的 Python 版），按同名字段搬过来，走同一套断言。

## 进程模型

- 单进程单写者（写操作串行，读并发，WAL + busy_timeout 10s）；同时监听 Unix socket（本机，免令牌）和 HTTP（127.0.0.1:19110，经 Nginx :9110 对外）。
- HTTP 上只读接口免令牌（网页用），写入和维护要 `X-L5-Token`；`import-sqlite` 只能走 socket。
