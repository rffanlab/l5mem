# L5 记忆库（第五层记忆）

[English](README.en.md)

给一组 AI 机器人 / 业务系统共用的**长期记忆库**：装不下上下文的、不该忘的、会过期的事实和判断，各归各位，随时查得到、改得了、追得回。

一个静态程序 `l5`（Go + SQLite FTS5，约 9 MB，零依赖），既是服务也是命令行；装成 `memo` 也能用。

- **快**：5 万条记忆，2 字高频词检索 p50 约 3 ms；命令行一次检索（含进程启动和 socket 往返）约 3 ms
- **中文友好**：中文按重叠二元组 + 三元组索引，不需要分词器；同义词组让「英伟达」也能查到「NVIDIA」
- **会过期**：每条记忆有生命周期（长期 / 缓变 / 快变），到期进待核对清单；世界变了就用新值**取代**旧值，旧值不删、随时回看
- **可审计**：谁在什么时候写的、谁核对过、被谁取代、为什么撤回，全部留痕；每天自动备份
- **一处写，到处用**：本机走 Unix socket（免令牌），远程走 HTTP（写入要令牌），网页只读浏览

设计说明见 [docs/DESIGN.md](docs/DESIGN.md)，基准见 [BENCH.md](BENCH.md)，
「拿新信息和长期记忆一起思考」的方法见 [docs/REFLECT_METHOD.md](docs/REFLECT_METHOD.md)。

## 记忆长什么样

| 字段 | 说明 |
|---|---|
| kind 类型 | 原则 principle / 决策 decision / 判断 judgment / 教训 lesson / 事实 fact / 备忘 memo |
| topic 主题 | 一句话主题；同一主题下的新值取代旧值，形成取代链 |
| body 正文 | 记忆本身 |
| tags 标签 | 检索捷径；查询词正好是标签名时直接顶到前面（太泛的「枢纽标签」自动放弃） |
| source 出处 | 链接或说明，方便回头核对 |
| volatility 生命周期 | stable 长期（不过期）/ slow 缓变（180 天核对一次）/ volatile 快变（30 天）；不填自动判断 |
| verified | 没有证据的写成未核实（7 天就要核对） |
| recheck_days | 可选：自定多少天后到期（例如「60 天内见分晓」的假设填 60） |

同样的内容重复写入不会重复存（内容哈希去重），所以写入可以放心重试。

## 快速上手

```bash
# 起服务（数据目录默认 /srv/e5-data/l5mem，这里换到 ./data）
l5 serve --data ./data --socket ./data/l5.sock --http 127.0.0.1:19110

# 另一个终端：写、查、改
export L5_SOCKET=./data/l5.sock
l5 add "GPU 租赁价格" "H100 云上租赁约 2 美元/小时" -t 算力,GPU -s https://example.com/report
l5 search 租赁价格 --explain           # 看每条为什么排在这里（各路名次、相关/邻近/新鲜/重要）
l5 add "GPU 租赁价格" "H100 云上租赁跌到约 1.6 美元/小时" --supersedes 1   # 新值取代旧值
l5 history 1                           # 取代链：旧值都还在
l5 due                                 # 到期该核对的
l5 verify 2 --note "对照了三家云厂商的价格页"
l5 relate 2                            # 联想：共享稀有标签的其他记忆（1~2 跳）
```

### 同义词组

```bash
l5 alias add 英伟达 NVIDIA NVDA        # 之后查任何一个写法，三个一起查
l5 search 英伟达财报                    # 中文串里包含别名也会展开：等于同时查「NVIDIA 财报」
l5 alias load deploy/aliases.txt       # 批量载入（自带 40 组 AI / 半导体行业常见写法）
l5 alias ls / l5 alias rm NVDA
```

用户直接敲的西文词按前缀匹配；同义扩展出来的西文写法按整词匹配，避免 `Arm` 带出 `army` 这类误伤。

### 更多检索选项

```bash
l5 search 算力 --since 7d              # 只看最近 7 天写入的（也可以 2026-11-01 / 2026-11-01 08:00:00 / 24h）
l5 search 算力 --until 2026-06-01      # 某个时间之前的（「当时我们怎么看」）
l5 search 算力 -k judgment --tag 假设   # 按类型、标签筛
l5 search 算力 --topic 关注方向/        # 按主题前缀
l5 search 算力 --history               # 连已被取代 / 撤回的一起看
l5 search 算力 --no-fold               # 关掉近似重复折叠
```

**近似重复折叠**：同一件事的几个版本、批量补录的同类事实，前 300 字的字对重合度 ≥ 60% 时，排在后面的挪到最后，
有限的名额留给不同的信息（只挪不删，名额够就还在）。

### 心头在想什么、新冒出来的连接

```bash
l5 mind              # 最近最常被写进、被引用、被想起的话题（越近越重；大词单列「背景」；末尾两成名额留给升得最快的）
l5 links             # 最近 7 天一起出现、以前从没一起出现过的两个话题（按「比偶然多了多少」排，大词排不上来）
l5 links --days 14 --prior 1    # 放宽：14 天内，以前最多一起出现过 1 次
l5 add "推论" "……" -k judgment --cites 12,15   # 这条是拿 #12、#15 推出来的：它们的话题算「被想起」一次
```

一个话题「在心头」的程度按 ACT-R 的基础激活算：每被用一次记一笔，按距今天数衰减，`A = ln Σ 次数 × (天数+1)^-0.5`，
昨天用 3 次比半年前用 30 次更在心头。只算真实发生的使用：写入（2 分）、被引用为推理依据（3 分）、被核对（2 分）、
出现在机器人检索结果的前 3 条（1 分）。网页浏览、巡检的检索不算，免得「越推荐越常被看、越常被看越推荐」。
太泛的大词（超过 8% 的记忆都挂着）和系统标签（每日沉思、ic-event-123 这类编号）不参与排名，系统标签可以用 `L5_MIND_STOP` 追加。

每日巡检报告里也附上心头前几个和新连接。InfoCollector 的每日沉思会拿新连接逐对判断是巧合、同一件事还是新关系，
新关系当成一条结论走自检、写回（写回时用 `cites` 标明依据）。

## 接入

### 命令行（本机或远程）

本机默认走 Unix socket（`/run/l5mem/l5.sock`，免令牌）。别的机器拷一份 `l5`，设置：

```bash
export L5_URL=http://<服务器>:9110 L5_TOKEN=<写入令牌> L5_CALLER=研究员
```

`L5_CALLER` 写上调用方的名字，使用日报里能区分是谁在用。读操作不需要令牌，写入和维护需要。

### Python

`clients/python/l5client.py` 只用标准库，拷进项目即可：

```python
from l5client import L5
m = L5(caller="infocollector")                       # 本机 socket；远程传 url= 和 token=
m.add("GPU 租赁价格", "H100 约 1.6 美元/小时", tags=["算力"], source="https://…")
for h in m.search("租赁价格", since="30d")["hits"]:
    print(h["id"], h["topic"], h["body"])
m.add_aliases("英伟达", "NVIDIA", "NVDA")
m.add("假设/某公司", "某公司下季度会下调指引", kind="judgment", unverified=True, recheck_days=60)
m.add("推论/电费", "……", kind="judgment", cites=[12, 15])   # 标明依据
for t in m.mind(limit=10)["tags"]:
    print(t["tag"], t["trend"], t["explore"])
for l in m.links(days=7):
    print(l["a"], "⟷", l["b"], l["count"])
```

### HTTP 接口

`/api/v1/<操作>`，GET 查询串或 POST JSON，返回 JSON。用请求头 `X-L5-Caller` 或参数 `caller` 标明身份；
写操作带 `X-L5-Token`（或 `Authorization: Bearer …`）。

| 操作 | 参数 | 说明 |
|---|---|---|
| add | topic, body, kind, tags[], source, volatility, unverified, supersedes[], meta{}, created_at, recheck_days, cites[] | 内容哈希去重：重复写返回 `existed: true` |
| search | q, limit, kind, tag, topic_prefix, since, until, history, explain, no_fold | 四路 RRF + 打分；返回 `expanded`（同义扩展）、`folded`（折叠条数） |
| mind | limit | 心头：`tags`（activation、trend new/up/flat/down、近 7 天 / 之前 4 周使用量、explore、最近 3 条）、`background`（大词） |
| links | days, min, prior, limit | 新连接：`links`（a、b、count、prior、pmi、score、entries） |
| aliases | GET 列出；POST `{"names": [...]}` 建组 / 合并；POST `{"name": x}` 移出 | 同义词组 |
| get / history / relate | id（history 也可 topic） | |
| supersede / retract / verify / volatility | old,new / id,reason / id,note / id,volatility | |
| due / tags / stats / metrics | days / limit / - / since_last, since_hours | |
| consolidate / doctor / repair / reclassify / watch | | 维护（watch = 每日巡检，没发现就 silent） |
| export / import | - / JSONL 正文 | 无损迁移：指纹 + 回读自检；同义词组一起导出导入 |

网页：服务地址根路径（检索、待核对、心头、标签、运行状态；只读），`?id=123` 直接打开某条，`?q=词` 直接检索，`?tab=mind` 直接看心头。

## 检索是怎么排的

1. **四路召回**（并行）：中文二元组 / 西文前缀（FTS5，每个词内部按同义词 OR、词与词之间 AND）、
   三元组子串（≥3 字的词）、主题前缀范围、标签点查。没有检索词时按时间列最新的
2. **RRF 融合**（k=60），再按 `0.45 相关 + 0.15 邻近 + 0.25 新鲜 + 0.15 重要 + 标签加分` 打分；
   邻近度只给前 3n 条算（看检索词在原文里是否真的连在一起）
3. **近似重复折叠**，取前 n 条

几个让它在 5 万条时仍然只要几毫秒的做法：bm25 只给值得排的那部分算；二元组索引用 `detail=full`；
候选的轻量元数据常驻内存；读路径不写库（访问计数在内存里攒着，定时回写）；检索结果 LRU 缓存，任何写入都会让缓存失效。
细节和实测见 [BENCH.md](BENCH.md)。

扩容设计（数据量大了以后怎么办）见 [docs/SCALING.md](docs/SCALING.md)。

## 维护

每天 `L5_WATCH_AT`（默认 10:17）自动巡检：

- 回写访问计数；逾期 7 天以上没核对的升级提醒
- 体检（索引一致性等）；使用日报增量（谁在用、p50/p95、标签命中率）
- **自动备份**：全库（含同义词组）导出到 `<数据目录>/backups/l5-日期.jsonl.gz`，保留最近 `L5_BACKUP_KEEP` 份
- 报告存 `reports/日期.json`；有发现时 POST 到 `L5_NOTIFY_URL`，没发现一字不发

恢复：`l5 import 备份.jsonl`（`.gz` 先解压），导入后会核对指纹并抽样回读自检。

## 编译与部署

```bash
./build.sh            # 需要 Go 1.22+ 和 gcc（mattn/go-sqlite3 是 cgo）；跑测试并生成静态链接的 bin/l5
VER=2026.11.19.1 ./build.sh
```

`bin/l5` 是静态链接的 x86_64 Linux 程序，拷到任何 Linux 都能跑。

`deploy/` 里是部署到一台 E5 管理的服务器的示例（systemd + Nginx + 注册到 E5 Manager）：
Windows 上运行 `deploy\deploy-158.bat`，会打包 `bin/l5` 和 `deploy/`、上传、执行 `deploy-l5mem.sh`
（幂等，失败自动回滚，不动数据）。首次部署生成写入令牌，保存在服务器的 `/etc/e5-runtime/l5mem.env`。

## 配置

| 变量 | 默认 | 说明 |
|---|---|---|
| L5_TOKEN | 空（部署脚本会生成） | HTTP 写入令牌；socket 不需要；空 = 不校验 |
| L5_DATA / L5_DB | /srv/e5-data/l5mem / 数据目录下 l5.db | 数据目录（报告、备份、使用日志）和数据库 |
| L5_SOCKET / L5_HTTP | /run/l5mem/l5.sock / 127.0.0.1:19110 | 监听地址（空字符串 = 不开） |
| L5_WATCH_AT | 10:17 | 每日巡检时间 |
| L5_BACKUP_KEEP | 14 | 每日备份保留几份；0 = 不备份 |
| L5_NOTIFY_URL | 空 | 巡检有发现时 POST JSON 到这里 |
| L5_W_LEX / L5_W_PROX / L5_W_REC / L5_W_IMP | 0.45 / 0.15 / 0.25 / 0.15 | 排序权重 |
| L5_HALF_LIFE_DAYS | 30 | 新鲜度半衰期 |
| L5_STABLE_FLAT | 1 | 长期记忆不随时间衰减；设 0 恢复统一衰减 |
| L5_MIND_STOP | 空 | 不参与心头和新连接的标签（逗号分隔，追加到默认的系统标签之后） |
| L5_TAG_BOOST | 0.10 | 查询词正好是标签名时的额外加分；使用日报判定「乱打标签」时设 0 |
| 客户端：L5_URL / L5_TOKEN / L5_CALLER | | 远程地址、写入令牌、调用方名字 |

## 目录

```
cmd/l5/            服务 + 命令行
cmd/l5bench/       基准（go run -tags sqlite_fts5 ./cmd/l5bench -sizes 1000,10000,50000）
internal/store/    存储、索引、检索、生命周期、同义词、导出导入
internal/server/   HTTP / socket 接口、每日巡检与备份、网页
internal/client/   Go 客户端
clients/python/    Python 客户端（只用标准库）
deploy/            部署脚本、systemd、Nginx、同义词表
docs/              设计、思考沉淀法
```
