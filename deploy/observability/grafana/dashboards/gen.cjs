// 生成 Grafana 仪表盘 JSON。
//
// 手写 400 行 JSON 太容易错一个逗号，而 Grafana 对坏 JSON 的报错
// 往往只说"failed to load dashboard"，什么都不告诉你。用脚本生成，
// 结构由代码保证，改动也只需改一处。
const DS = { type: 'prometheus', uid: 'deeptalk-prom' }

let id = 0
const panels = []

/** 面板通用外壳 */
function panel({ title, desc, type = 'timeseries', w = 12, h = 8, targets, unit, decimals, thresholds, mappings, max }) {
  id += 1
  const defaults = { unit: unit || 'short', custom: { drawStyle: 'line', lineWidth: 2, fillOpacity: 12, showPoints: 'never' } }
  if (decimals !== undefined) defaults.decimals = decimals
  if (max !== undefined) defaults.max = max
  if (thresholds) defaults.thresholds = { mode: 'absolute', steps: thresholds }
  if (mappings) defaults.mappings = mappings
  return {
    id,
    type,
    title,
    description: desc,
    datasource: DS,
    gridPos: { h, w, x: gridPosX, y: gridPosY },
    fieldConfig: { defaults, overrides: [] },
    options: {
      legend: { displayMode: 'list', placement: 'bottom', showLegend: true },
      tooltip: { mode: 'multi', sort: 'desc' }
    },
    targets
  }
}

/** stat 面板用更紧凑的图例 */
function stat(opts) {
  const p = panel({ ...opts, type: 'stat', h: opts.h || 5, w: opts.w || 4 })
  p.options = {
    reduceOptions: { calcs: ['lastNotNull'], fields: '', values: false },
    colorMode: 'value',
    graphMode: opts.graphMode || 'area',
    justifyMode: 'auto',
    textMode: opts.textMode || 'auto'
  }
  return p
}

function ts(opts) {
  return panel({ ...opts, type: 'timeseries' })
}

const expr = (e, legend, refId) => ({ refId: refId || 'A', datasource: DS, expr: e, legendFormat: legend, editorMode: 'code', range: true })

/**
 * guarded 给"比率"挂一个"分母必须有量"的条件。
 *
 * 为什么不能简单相除、也不能用 clamp_min 兜底：
 *
 *   直接除            0/0 = NaN       → Grafana 显示 "NaN"
 *   clamp_min 兜底    0/0.001 = 0     → 看起来是个正常数字，却是假的
 *
 * 第二种更危险。上线初期那个"澄清流失率 100%"就是它算出来的：
 * 一条澄清都没发生过，分母被兜成 0.001，于是 0/0.001=0、1-0=1。
 * 同理"工具成功率"会显示 0%，看着像全线故障。
 * **没人会去查一个看起来有值的面板** —— 假数据比没数据害人得多。
 *
 * `and (分母 > 0)` 让整条表达式在没有任何样本时返回空，
 * Grafana 显示 No data。那才是诚实的答案：
 * "没发生过"和"发生了 0 次"不是一回事。
 */
const guarded = (ratio, denom) => `(\n  ${ratio}\n)\nand\n(${denom} > 0)`

/**
 * quantile 给 histogram_quantile 挂同样的"必须有样本"条件。
 *
 * 为什么必须挂：直方图序列**存在但窗口内没有任何观测**时，
 * histogram_quantile 不会返回空，而是返回 **NaN** —— Grafana 上就是一个
 * 大写的 "NaN" 杵在面板中间，看着像坏了。
 *
 * 实测踩到过：工具调用耗时 p95 在调用发生 5 分钟之后显示 NaN。
 * 那 5 分钟里确实没有样本，"无样本"应当显示 No data 而不是 NaN。
 *
 * total 传"按观测主体聚合的所有桶之和"：每条观测都会落进它自己及以上的所有桶，
 * 所以桶之和 > 0 等价于"窗口内有观测"。
 */
const quantile = (q, buckets, total) =>
  `histogram_quantile(${q}, ${buckets})\nand\n(${total} > 0)`

// 手工排布：每行 y 递增，x 在 0/6/12/18 之间走
let gridPosX = 0
let gridPosY = 0
const layout = []

function row(y) { gridPosY = y; gridPosX = 0 }

// ==================== 第一行：一眼看健康 ====================
row(0)
gridPosX = 0
panels.push(stat({
  title: '请求速率', w: 4,
  desc: '所有 AI 请求的每秒速率（含缓存命中）',
  unit: 'reqps', decimals: 2,
  targets: [expr('sum(rate(deeptalk_ai_requests_total[5m]))', '全部请求')]
}))
gridPosX = 4
panels.push(stat({
  title: '失败率', w: 4,
  desc: 'status != ok 的占比。含 quota_exceeded（配额拦截是策略，不一定是故障）',
  unit: 'percentunit', decimals: 2, max: 1,
  thresholds: { mode: 'absolute', steps: [{ color: 'green', value: null }, { color: 'yellow', value: 0.02 }, { color: 'red', value: 0.1 }] },
  targets: [expr(guarded(
    'sum(rate(deeptalk_ai_requests_total{status!="ok"}[5m])) / sum(rate(deeptalk_ai_requests_total[5m]))',
    'sum(rate(deeptalk_ai_requests_total[5m]))'), '失败率')]
}))
gridPosX = 8
panels.push(stat({
  title: 'Agent 工具成功率', w: 4,
  desc: 'status="ok" 的占比。rejected（越界、熔断、配额）单独计，不算故障——否则模型反复试探越界路径会让它看起来像工具坏了',
  unit: 'percentunit', decimals: 1, max: 1,
  thresholds: { mode: 'absolute', steps: [{ color: 'red', value: null }, { color: 'yellow', value: 0.8 }, { color: 'green', value: 0.95 }] },
  targets: [expr(guarded(
    'sum(rate(deeptalk_agent_tool_calls_total{status="ok"}[5m])) / sum(rate(deeptalk_agent_tool_calls_total[5m]))',
    'sum(rate(deeptalk_agent_tool_calls_total[5m]))'), '成功率')]
}))
gridPosX = 12
panels.push(stat({
  title: '澄清触发率', w: 4,
  desc: '每轮对话里 Agent 决定反问用户的比例。太高说明提示词没收紧，太低说明该问的没问',
  unit: 'percentunit', decimals: 1, max: 1,
  targets: [expr(guarded(
    'sum(rate(deeptalk_agent_clarify_total{outcome="asked"}[5m])) / sum(rate(deeptalk_agent_turns_total[5m]))',
    'sum(rate(deeptalk_agent_turns_total[5m]))'), '触发率')]
}))
gridPosX = 16
panels.push(stat({
  title: '澄清流失率', w: 4,
  desc: '问了但用户没答的比例。这个数字决定澄清功能该不该留。没有澄清发生时显示 No data —— 0 次澄清的流失率是"无定义"，不是 100%',
  unit: 'percentunit', decimals: 1, max: 1,
  thresholds: { mode: 'absolute', steps: [{ color: 'green', value: null }, { color: 'yellow', value: 0.3 }, { color: 'red', value: 0.6 }] },
  targets: [expr(guarded(
    '1 - sum(rate(deeptalk_agent_clarify_total{outcome="answered"}[5m])) / sum(rate(deeptalk_agent_clarify_total{outcome="asked"}[5m]))',
    'sum(rate(deeptalk_agent_clarify_total{outcome="asked"}[5m]))'), '流失率')]
}))
gridPosX = 20
panels.push(stat({
  title: '本地连接器', w: 4,
  desc: '在线的工作区连接器数量',
  unit: 'short', decimals: 0,
  thresholds: { mode: 'absolute', steps: [{ color: 'text', value: null }, { color: 'green', value: 1 }] },
  targets: [expr('max(deeptalk_local_agent_online)', '在线')]
}))

// ==================== 第二行：Agent 效率 ====================
row(5)
gridPosX = 0
panels.push(ts({
  title: 'ReAct 步数分布（每轮调用模型几次）',
  desc: 'Agent 效率的核心口径。长尾变厚 = 模型在空转（工具描述写得不好，它在瞎试）。注意：步数取自模型调用次数，不是工具调用次数——只想要不想做的 Agent 工具调用很少，那种空转只有看步数才看得出来',
  unit: 'short', w: 12,
  targets: [
    expr(quantile('0.50', 'sum by (le) (rate(deeptalk_agent_steps_per_turn_bucket[15m]))',
      'sum (rate(deeptalk_agent_steps_per_turn_bucket[15m]))'), 'p50'),
    expr(quantile('0.95', 'sum by (le) (rate(deeptalk_agent_steps_per_turn_bucket[15m]))',
      'sum (rate(deeptalk_agent_steps_per_turn_bucket[15m]))'), 'p95', 'B'),
    expr(quantile('0.99', 'sum by (le) (rate(deeptalk_agent_steps_per_turn_bucket[15m]))',
      'sum (rate(deeptalk_agent_steps_per_turn_bucket[15m]))'), 'p99', 'C')
  ]
}))
gridPosX = 12
panels.push(ts({
  title: '单轮耗时',
  desc: '一轮对话从进门到出答案的总时长（含所有工具往返）',
  unit: 's', w: 12,
  targets: [
    expr(quantile('0.50', 'sum by (le) (rate(deeptalk_agent_turn_duration_seconds_bucket[5m]))',
      'sum (rate(deeptalk_agent_turn_duration_seconds_bucket[5m]))'), 'p50'),
    expr(quantile('0.95', 'sum by (le) (rate(deeptalk_agent_turn_duration_seconds_bucket[5m]))',
      'sum (rate(deeptalk_agent_turn_duration_seconds_bucket[5m]))'), 'p95', 'B'),
    expr(quantile('0.99', 'sum by (le) (rate(deeptalk_agent_turn_duration_seconds_bucket[5m]))',
      'sum (rate(deeptalk_agent_turn_duration_seconds_bucket[5m]))'), 'p99', 'C')
  ]
}))

// ==================== 第三行：工具 ====================
row(13)
gridPosX = 0
panels.push(ts({
  title: '工具调用速率（按工具与结局）',
  desc: 'status 的四种值：ok / error（意外）/ timeout / rejected（按策略拒绝：路径越界、熔断打开）。rejected 单列是刻意的，混进 error 会让成功率失去意义',
  unit: 'ops', w: 12,
  targets: [expr('sum by (tool, status) (rate(deeptalk_agent_tool_calls_total{tool!="none"}[5m]))', '{{tool}} · {{status}}')]
}))
gridPosX = 12
panels.push(ts({
  title: '工具调用耗时 p95',
  desc: '按工具分开看。某个工具忽然变慢，通常在它自己的上游，而不是 Agent 循环',
  unit: 's', w: 12,
  targets: [expr(quantile('0.95',
    'sum by (tool, le) (rate(deeptalk_agent_tool_duration_seconds_bucket{tool!="none"}[5m]))',
    'sum by (tool) (rate(deeptalk_agent_tool_duration_seconds_bucket{tool!="none"}[5m]))'), '{{tool}} p95')]
}))

// ==================== 第四行：模型与成本 ====================
row(21)
gridPosX = 0
panels.push(ts({
  title: '模型调用延迟',
  desc: 'Agent 内部每一次模型调用的耗时（一轮可能有多次）',
  unit: 's', w: 12,
  targets: [
    expr(quantile('0.50', 'sum by (model, le) (rate(deeptalk_agent_model_duration_seconds_bucket{model!="none"}[5m]))',
      'sum by (model) (rate(deeptalk_agent_model_duration_seconds_bucket{model!="none"}[5m]))'), '{{model}} p50'),
    expr(quantile('0.95', 'sum by (model, le) (rate(deeptalk_agent_model_duration_seconds_bucket{model!="none"}[5m]))',
      'sum by (model) (rate(deeptalk_agent_model_duration_seconds_bucket{model!="none"}[5m]))'), '{{model}} p95', 'B')
  ]
}))
gridPosX = 12
panels.push(ts({
  title: 'Token 速率（按类型）',
  desc: 'cached = 命中上游前缀缓存的那部分输入。它按 1/10 计费，所以这一条涨通常是好事',
  unit: 'short', w: 6,
  targets: [expr('sum by (kind) (rate(deeptalk_ai_tokens_total[5m]))', '{{kind}}')]
}))
gridPosX = 18
panels.push(techCostPanel())

function techCostPanel() {
  return ts({
    title: '费用（元/小时）',
    desc: '把微元累计速率换算成每小时的元，比看累计值直观。命中上游前缀缓存的输入 token 已按 1/10 计过价，所以这条曲线本身就是折后价',
    unit: 'currencyCNY', w: 6,
    targets: [expr('sum(rate(deeptalk_ai_cost_micros_total[5m])) / 1e6 * 3600', '全部模型')]
  })
}

// ==================== 第五行：稳定性与质量 ====================
row(29)
gridPosX = 0
panels.push(ts({
  title: '缓存命中率（按层级）',
  desc: 'layer：exact（L1 精确）/ semantic（L2 语义）/ negative（空结果冷却）。命中率用 hit/(hit+miss) 现算，不用后端那个 gauge——多副本时各自算的 gauge 会互相覆盖',
  unit: 'percentunit', decimals: 2, max: 1, w: 8,
  targets: [expr(guarded(
    'sum by (level) (rate(deeptalk_cache_hit_total[5m])) / (sum by (level) (rate(deeptalk_cache_hit_total[5m])) + sum by (level) (rate(deeptalk_cache_miss_total[5m])))',
    '(sum by (level) (rate(deeptalk_cache_hit_total[5m])) + sum by (level) (rate(deeptalk_cache_miss_total[5m])))'), '{{level}}')]
}))
gridPosX = 8
panels.push(ts({
  title: '熔断器状态',
  desc: '0=closed 1=half-open 2=open。任何一条跳到 2 都值得立刻看',
  unit: 'short', decimals: 0, w: 8,
  targets: [expr('deeptalk_circuit_breaker_state', '{{name}}')]
}))
gridPosX = 16
panels.push(ts({
  title: '评测得分趋势',
  desc: '来自 cmd/eval -metricsOut。分数本身是数字，**趋势**才是信息：一次 0.74 说明不了什么，连着 0.74 → 0.70 → 0.66 说明检索在退化',
  unit: 'percentunit', decimals: 2, max: 1, w: 8,
  targets: [expr('deeptalk_eval_score', '{{metric}}')]
}))

// ==================== 第六行：上下文与轨迹 ====================
row(37)
gridPosX = 0
panels.push(ts({
  title: '上下文 token 构成',
  desc: 'kind：system / history / rag / tool / question。总和逼近模型上限而效果没提升，就是该做压缩或截断的信号',
  unit: 'short', w: 12,
  targets: [expr('deeptalk_agent_context_tokens', '{{kind}}')]
}))
gridPosX = 12
panels.push(ts({
  title: '轨迹步骤速率（按类型）',
  desc: 'kind：turn / model / tool / retrieve / clarify / cache。轨迹是"这一轮到底怎么了"的唯一答案——指标只说整体在变差',
  unit: 'ops', w: 6,
  targets: [expr('sum by (kind) (rate(deeptalk_agent_trace_spans_total[5m]))', '{{kind}}')]
}))
gridPosX = 18
panels.push(ts({
  title: '轨迹落库结果',
  desc: '落库失败不该影响对话（观测设施不能成为故障点），但要能看见',
  unit: 'ops', w: 6,
  targets: [expr('sum by (result) (rate(deeptalk_agent_trace_persist_total[5m]))', '{{result}}')]
}))

// ==================== 第七行：推理调度 ====================
//
// 这一整块之前是**看不见的**：指标从调度器建起来那天就在吐，
// 但盘上没有任何面板。结果是"池子满了、请求在排队、开始拒绝"这件事
// 只有翻日志才知道 —— 而它恰恰是最需要提前看到的一类问题。
row(45)
gridPosX = 0
panels.push(ts({
  title: '在途 vs 排队',
  desc: 'inflight 逼近 maxConcurrent、同时 queue_depth 抬头 = 池子开始不够用。两条线一起看才有意义：只看其中一个分不清"忙但顺畅"和"忙到堵住"',
  unit: 'short', w: 12,
  targets: [
    expr('sum by (model) (deeptalk_inference_inflight)', '{{model}} 在途'),
    expr('sum by (model) (deeptalk_inference_queue_depth)', '{{model}} 排队', 'B')
  ]
}))
gridPosX = 12
panels.push(ts({
  title: '排队等待时长',
  desc: '从入队到拿到槽位的时间。它逼近 queueTimeout 就说明"有效队列深度"已经用满 —— 再来的请求只会排队等死',
  unit: 'ms', w: 12,
  targets: [
    expr(quantile('0.50', 'sum by (model, le) (rate(deeptalk_inference_queue_wait_ms_bucket[5m]))',
      'sum by (model) (rate(deeptalk_inference_queue_wait_ms_bucket[5m]))'), '{{model}} p50'),
    expr(quantile('0.95', 'sum by (model, le) (rate(deeptalk_inference_queue_wait_ms_bucket[5m]))',
      'sum by (model) (rate(deeptalk_inference_queue_wait_ms_bucket[5m]))'), '{{model}} p95', 'B')
  ]
}))

// ==================== 第八行：拒绝与准入 ====================
row(53)
gridPosX = 0
panels.push(ts({
  title: '拒绝原因',
  desc: 'queue_full=队列满 / queue_timeout=排队超时 / breaker_open=准入熔断 / shutting_down=优雅关闭。**四种原因的处置完全不同**：队列满要提高并发，排队超时要缩队列，熔断要查下游',
  unit: 'ops', w: 12,
  targets: [expr('sum by (model, reason) (rate(deeptalk_inference_rejected_total[5m]))', '{{model}} · {{reason}}')]
}))
gridPosX = 12
panels.push(ts({
  title: '准入熔断状态',
  desc: '这是**调度层**的熔断（连续失败 N 次就在入队前快速拒绝），和上面 resilience 那个按失败率的模型熔断是两套，解决不同问题。0=closed 1=half-open 2=open',
  unit: 'short', decimals: 0, w: 12,
  targets: [expr('deeptalk_inference_breaker_state', '{{model}}')]
}))

const dashboard = {
  title: 'DeepTalk · AI 应用观测',
  uid: 'deeptalk-overview',
  description: '容量视角（请求/token/费用）+ 效果视角（工具成功率/步数分布/澄清触发率）。后半部分是 AI 应用区别于普通后端的地方。',
  tags: ['deeptalk', 'ai-agent', 'llm'],
  schemaVersion: 39,
  version: 1,
  refresh: '30s',
  time: { from: 'now-6h', to: 'now' },
  timezone: 'browser',
  editable: true,
  graphTooltip: 1,
  panels,
  templating: { list: [] },
  annotations: { list: [{ name: 'Annotations', datasource: { type: 'grafana', uid: '-- Grafana --' }, enable: true, iconColor: 'rgba(0, 211, 255, 1)', target: { limit: 100, matchAny: false, tags: [], type: 'dashboard' } }] }
}

console.log(JSON.stringify(dashboard, null, 2))
