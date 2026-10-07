# 用 Prometheus + Grafana 看 DeepTalk

这一份只起**监控栈**，不碰业务容器。观测是可选的，不该让只想跑个对话的人
被迫拉起两个加起来快 1G 的镜像。

---

## 一、你需要下载什么

**什么都不用额外装。** Docker 会自动拉这两个镜像：

| 组件 | 镜像 | 大小 |
|---|---|---|
| Prometheus | `prom/prometheus:v3.1.0` | ~100 MB |
| Grafana | `grafana/grafana:11.5.1` | ~350 MB |

前提是你的 WSL 能上网。首次 `up -d` 会花几分钟拉镜像，之后就都是本地缓存了。

---

## 二、先搞清楚：`host.docker.internal` 指向谁

**这是整件事唯一容易踩的坑，先看这一节。**

Prometheus 跑在容器里，它要抓的 DeepTalk 后端在容器外面。而"容器外面"
在你的拓扑里可能是**两个不同的地方**：

```
你的机器
├── Windows                    ← 后端在这里跑（go run / IDE）
└── WSL2 Ubuntu
    ├── Docker Engine          ← Prometheus / Grafana 在这里（容器）
    └── （也可能：后端在这里跑）
```

`extra_hosts: host.docker.internal:host-gateway` 里的 **host-gateway 解析到的是
"Docker 守护进程所在的那台机器"** —— Docker 在 WSL 里，所以它指向 **WSL**，
**不是 Windows**。

| 后端跑在哪 | 目标地址 |
|---|---|
| **WSL 里**（和 Docker 同机） | `host.docker.internal:9090` |
| **Windows 上** | Windows 在 WSL 眼里的地址 = 默认网关 |

你的是**第二种**。已经配好了：

```
Windows 在 WSL 眼里的地址 = 172.24.32.1
```

### 这个地址为什么会变，以及怎么不用管它

在 WSL2 的 NAT 模式下，**每次重启 WSL 都可能换一个网段**。写死在配置里的话，
某天 Prometheus 的 Targets 页面会显示 DOWN，而你会以为是后端挂了 ——
**静默失效是最难查的一类问题**。

所以目标地址没有写在 `prometheus.yml` 里，而是放在
[`targets/deeptalk.yml`](targets/deeptalk.yml)，用 Prometheus 的
`file_sd_configs` 读取。文件一变，Prometheus 30 秒内自动重载，
不用改配置、不用重启、不用调 `/-/reload`。

地址变了（或者 Targets 显示 DOWN）就跑一下：

```bash
bash deploy/observability/refresh-target.sh
```

它会自己 `ip route show default` 拿到当前网关、**先测通再写文件**，
连不上还会按可能性列出排查步骤：

```
Windows 地址（WSL 视角）: 172.24.32.1
✓ 后端可达：http://172.24.32.1:9090/metrics
✓ 已写入 .../targets/deeptalk.yml

Prometheus 会在 30 秒内自动重载，不用重启容器。
查看效果： http://localhost:9091/targets
```

想让它在开终端时自动跑，把这行加到 `~/.bashrc`：

```bash
[ -f ~/DeepTalk/deploy/observability/refresh-target.sh ] && \
  bash ~/DeepTalk/deploy/observability/refresh-target.sh --quiet
```

### 连不上的话，按这个顺序排查

脚本会替你列出这四条，提前写在这里方便你对照：

1. **后端没在跑** —— 在 Windows 上确认 `go run ./cmd/server` 还活着
2. **后端只监听了 127.0.0.1** —— 必须监听 `0.0.0.0`（本项目默认就是，日志里会打印
   `HTTP server listening on 0.0.0.0:9090`）
3. **Windows 防火墙拦了来自 WSL 网段的入站** —— 在**管理员** PowerShell 里执行：

   ```powershell
   New-NetFirewallRule -DisplayName 'DeepTalk metrics from WSL' `
     -Direction Inbound -LocalPort 9090 -Protocol TCP -Action Allow -Profile Any
   ```

   想撤销：`Remove-NetFirewallRule -DisplayName 'DeepTalk metrics from WSL'`

4. **WSL 换了网段** —— 重跑 `refresh-target.sh`

> 想彻底摆脱这个地址问题：把后端也放进 WSL 跑（`go run ./cmd/server` 在 WSL 里执行，
  它的数据源 MySQL/Redis 都在 Windows 上也能连过去），或者开 WSL2 的 mirrored
> networking（Windows 11 22H2+，`%UserProfile%\.wslconfig` 里加
> `networkingMode=mirrored`，之后两边共享 localhost）。

---

## 三、四步跑起来

在 **WSL 终端**里，`cd` 到项目根目录：

```bash
# 0) 先校验配置，并确认目标地址是对的（不用 Docker，秒级）
go run ./deploy/observability/checkyaml

# 0.5) 刷新"Windows 在 WSL 眼里的地址"，并测试后端可达性
bash deploy/observability/refresh-target.sh

# 1) 起栈（首次会拉镜像，约 450MB）
docker compose -f deploy/observability/docker-compose.yml up -d

# 2) 看一眼两个容器都健康
docker compose -f deploy/observability/docker-compose.yml ps

# 3) 日志里不该有报错
docker compose -f deploy/observability/docker-compose.yml logs --tail=20
```

对应地址：

| | 地址 | 账号 |
|---|---|---|
| Prometheus | http://localhost:9091 | 无 |
| Grafana | http://localhost:3000 | `admin` / `admin` |

> 为什么 Prometheus 是 **9091** 不是默认的 9090：**DeepTalk 后端占着 9090**。
> 两个都想用 9090 的话，谁后起谁失败。

如果 Windows 浏览器打不开 `localhost:3000`，那说明 WSL2 的端口转发没生效。
在 WSL 里执行 `hostname -I` 拿到 WSL 的 IP，用 `http://<那个IP>:3000` 试试。

> **第 0.5 步不要跳过**：它会在写文件之前先连一次后端。
> 如果这里就报"连不上"，那 Prometheus 一定也是 DOWN，
> 而那时你已经有一份排查清单了，不用去猜。

---

## 四、验证 Prometheus 真的抓到了

打开 http://localhost:9091 → 顶部菜单 **Status → Targets**。

`deeptalk` 这个 job 的 State 必须是 **UP**。

- **UP** → 往下走
- **DOWN** → target 写错了，回到第二节。鼠标悬停在 Error 上会告诉你原因
  （`connection refused` = 地址对但后端没跑；`no route to host` / 超时 = 地址不对）

确认抓到了数据，在查询框里敲：

```promql
deeptalk_agent_tool_calls_total
```

点 Execute，应该能看到带 `tool` / `status` 标签的序列。
**看不到的话先别去 Grafana** —— Prometheus 里没有的东西，Grafana 画不出来。

---

## 五、看 Grafana

1. 浏览器打开 http://localhost:3000
2. 用 `admin` / `admin` 登录（会提示改密码，可以点 Skip）
3. 左侧 **Dashboards** → 文件夹 **DeepTalk** → **DeepTalk · AI 应用观测**

数据源和仪表盘都是**声明式预置**的（`grafana/provisioning/`），
不需要手点 "Add data source" 或手搓面板。这也是为什么这份配置值得进 git ——
换台机器 `up -d` 一下，同一个盘就回来了。

---

## 六、19 个面板在看什么

### 第一行：一眼看健康

| 面板 | 说明 |
|---|---|
| **请求速率** | 所有 AI 请求的每秒速率（含缓存命中） |
| **失败率** | `status != ok` 的占比 |
| **Agent 工具成功率** | `status="ok"` 的占比。**`rejected` 单独计，不算故障** |
| **澄清触发率** | Agent 决定反问用户的比例。太高说明提示词没收紧 |
| **澄清流失率** | 问了但用户没答的比例 —— 决定澄清功能该不该留 |
| **本地连接器** | 在线的工作区连接器数量 |

**工具成功率为什么要把 `rejected` 摘出来**：路径越界、熔断打开、配额拦截
都是"按策略正确拒绝"。混进 `error` 的话，一个模型反复试探越界路径的会话，
在盘上看起来会像是工具大面积故障。

### 第二行：Agent 效率（这行最能体现 AI 应用功力）

| 面板 | 说明 |
|---|---|
| **ReAct 步数分布** | 每轮调用模型几次的 p50/p95/p99 |
| **单轮耗时** | 一轮对话从进门到出答案，含所有工具往返 |

**步数长尾变厚 = 模型在空转**，通常是工具描述写得不好、它在瞎试。

> 口径提醒：步数取自**模型调用次数**，不是工具调用次数。
> 一个只想要不想做的 Agent 工具调用很少 —— 那种空转只有看步数才看得出来。
> 实测过一次：`list_files` 被连调 4 次、每次 2ms 都成功，最后撞满 maxStep。
> 工具成功率 100%，但 Agent 完全没干活。

### 第三行：工具

| 面板 | 说明 |
|---|---|
| **工具调用速率（按工具与结局）** | `ok` / `error` / `timeout` / `rejected` 四条分开 |
| **工具调用耗时 p95** | 按工具分开。某个工具忽然变慢，问题通常在它自己的上游 |

### 第四行：模型与成本

| 面板 | 说明 |
|---|---|
| **模型调用延迟** | Agent 内部每一次模型调用的耗时（一轮可能有多次） |
| **Token 速率** | `prompt` / `completion` / `cached` |
| **费用（元/小时）** | 微元累计速率换算成每小时的元，比看累计值直观 |

`cached` 是命中**上游前缀缓存**的那部分输入，按 1/10 计费 —— 所以这条涨通常是好事。

### 第五行：稳定性与质量

| 面板 | 说明 |
|---|---|
| **缓存命中率（按层级）** | `exact` / `semantic` / `negative` |
| **熔断器状态** | 0=closed 1=half-open 2=open。**任何一条跳到 2 都值得立刻看** |
| **评测得分趋势** | 来自 `cmd/eval -metricsOut` |

**命中率为什么在 Grafana 里现算**（`hit/(hit+miss)`）而不是直接用后端那个
`deeptalk_cache_hit_rate` gauge：多副本时每个进程各算各的，会互相覆盖。
从两个 counter 现算，Prometheus 会自动把副本聚合起来 —— 这也是
**counter 比 gauge 更适合做比率**的一般道理。

**评测分为什么值得进 Prometheus**：分数本身是数字，**趋势**才是信息。
一次 0.74 说明不了什么；连着 0.74 → 0.70 → 0.66 说明检索在退化。

### 第六行：上下文与轨迹

| 面板 | 说明 |
|---|---|
| **上下文 token 构成** | `system` / `history` / `rag` / `tool` / `question` |
| **轨迹步骤速率** | 按 `turn` / `model` / `tool` / `retrieve` / `clarify` / `cache` |
| **轨迹落库结果** | 落库失败不该影响对话，但要能看见 |

总和逼近模型上限而效果没提升，就是该做压缩或截断的信号。

---

## 七、指标只有整体趋势，单轮细节看轨迹

Grafana 告诉你**整体在变差**，但不会告诉你**这一轮到底怎么了**。
后者看轨迹接口：

```bash
# 最近的失败轮次
curl -H "Authorization: Bearer <JWT>" http://localhost:9090/api/v1/AI/trace

# 某一轮的完整步骤
curl -H "Authorization: Bearer <JWT>" http://localhost:9090/api/v1/AI/trace/<trace_id>
```

返回的 `steps` 是 Agent 每一步的流水账（模型调用 / 工具调用 / 耗时 / 结局）。

---

## 八、把评测结果也接进来（可选）

```bash
go run ./cmd/eval -set eval/golden_example.json -user <账号> -type 2 \
  -metricsOut eval.prom
```

`eval.prom` 是 Prometheus 文本格式。让它变成时间序列有两种接法：

**Pushgateway**（一次性任务的标准做法）：

```bash
docker run -d -p 9092:9091 --name deeptalk-pushgateway prom/pushgateway:v1.9.0
cat eval.prom | docker run --rm -i --network host curlimages/curl:latest \
  --data-binary @- http://localhost:9092/metrics/job/deeptalk-eval
```

然后在 `prometheus.yml` 里加：

```yaml
  - job_name: pushgateway
    honor_labels: true
    static_configs:
      - targets: ["pushgateway:9091"]
```

（需要把 pushgateway 也加进 compose 的网络里。）

---

## 九、改配置 / 关掉

```bash
# 后端地址变了（重启过 WSL / Targets 显示 DOWN）—— 不用碰配置文件
bash deploy/observability/refresh-target.sh

# 改完 prometheus.yml（抓取间隔、加 job 之类）热加载，不重启
docker exec deeptalk-prometheus wget -qO- --post-data='' http://localhost:9090/-/reload

# 改完 Grafana 的 provisioning 要重启 grafana
docker compose -f deploy/observability/docker-compose.yml restart grafana

# 停掉（保留数据）
docker compose -f deploy/observability/docker-compose.yml down

# 停掉并删掉历史数据
docker compose -f deploy/observability/docker-compose.yml down -v

# 改完仪表盘 JSON 让 Grafana 重新读
# 仪表盘是挂载进去的，改完等 30 秒自动重载，或者重启 grafana
```

注意 `targets/` 下的文件**不需要** `/-/reload`：`file_sd` 是 Prometheus
自己去轮询的（`refresh_interval: 30s`）。这正是用它而不是 `static_configs` 的原因。

Prometheus 默认保留 **15 天**历史（`--storage.tsdb.retention.time=15d`）。

---

## 十、文件清单

```
deploy/observability/
├── docker-compose.yml                    # prometheus + grafana
├── prometheus.yml                        # 抓取配置（目标地址在 targets/ 里）
├── refresh-target.sh                     # 刷新"Windows 在 WSL 眼里的地址"
├── targets/
│   └── deeptalk.yml                       # ★ 目标地址在这里，由上面那个脚本维护
├── README.md                             # 就是本文件
├── checkyaml/main.go                     # 起容器前先校验这几份 YAML
└── grafana/
    ├── provisioning/
    │   ├── datasources/prometheus.yml    # 数据源声明式配置
    │   └── dashboards/dashboards.yml     # 告诉 Grafana 去哪读面板
    └── dashboards/
        ├── deeptalk.json                 # 19 个面板（真正被 Grafana 读的文件）
        └── gen.cjs                       # 生成上面那个 JSON 的脚本
```

### 起容器之前，先校验配置

```bash
go run ./deploy/observability/checkyaml
```

它不只是验"YAML 能解析"，还会把**当前生效的抓取目标**打出来：

```
  ✓ .../prometheus.yml
      scrape_interval = 15s
      job deeptalk     -> file_sd [/etc/prometheus/targets/*.yml]
        └─ 当前目标: [172.24.32.1:9090]
      job prometheus   -> [localhost:9090]
```

这一步很值：`static_configs` 缩进错一格照样能解析成功，只是 `targets` 变成空 ——
那种配置 **Prometheus 起得来但抓不到任何东西**，表现和"后端没跑"一模一样，最难查。

### 为什么要有个 gen.cjs

手写 400 行面板 JSON，错一个逗号就全盘打不开，而 Grafana 只会说
`failed to load dashboard` —— 什么都不告诉你。用脚本生成，结构由代码保证。

改面板的方式（选一个）：

```bash
# A. 直接在 Grafana 界面里改（配置里开了 allowUiUpdates）
#    改完仪表盘会自动写回 deeptalk.json

# B. 改 gen.cjs 再重新生成
node deploy/observability/grafana/dashboards/gen.cjs \
  > deploy/observability/grafana/dashboards/deeptalk.json
```

用 B 的话注意：A 在界面上改的内容会被覆盖。两者选一个当权威版本。
