// DeepTalk 压测脚本（k6）
//
// 运行前必须先跑：go run ./stress/prep -users 50 -out stress/users.json
//
// 用 SCENARIO 环境变量选择场景：
//
//	k6 run -e SCENARIO=local   stress/k6/chat.js   # 纯本地接口，不碰模型
//	k6 run -e SCENARIO=multi   stress/k6/chat.js   # 多会话并发（真实吞吐，主力场景）
//	k6 run -e SCENARIO=same    stress/k6/chat.js   # 同会话并发（测会话锁串行化）
//	k6 run -e SCENARIO=repeat  stress/k6/chat.js   # 重复问题（测意图/语义/前缀三层缓存）
//	k6 run -e SCENARIO=long    stress/k6/chat.js   # 长历史（触发记忆压缩的同步 LLM 调用）
//	k6 run -e SCENARIO=stream  stress/k6/chat.js   # SSE 流式（额外看首字节延迟 TTFB）
//	k6 run -e SCENARIO=mixed   stress/k6/chat.js   # 混合流量（最接近真实）
//
// 常用环境变量：
//
//	-e VUS=20          并发虚拟用户数（默认按场景给）
//	-e DURATION=2m     稳态时长
//	-e BASE_URL=...    覆盖被测地址（默认取 users.json 里的）
//	-e USERS=50        使用 users.json 里的前 N 个账号
//	-e LONG_PAD=1200   长历史场景里问题的填充长度（字符，上限 4000）
//
// 结果判定：DeepTalk 的业务成功码是 status_code=1000。
// 注意「业务失败」和「限流 429」是两回事，脚本分开计数，别混在一个错误率里看。

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';

// ---------------- 读预生成数据 ----------------

const data = JSON.parse(open('../users.json'));

const ENV = {
  baseUrl: __ENV.BASE_URL || data.baseUrl || 'http://127.0.0.1:9090',
  apiPrefix: data.apiPrefix || '/api/v1/AI',
  modelType: data.modelType || '2',
  userCount: parseInt(__ENV.USERS || String(data.users.length), 10),
  longPad: parseInt(__ENV.LONG_PAD || '1200', 10),
};

const users = data.users.slice(0, ENV.userCount);
if (users.length === 0) {
  throw new Error('users.json 里没有账号，请先运行 go run ./stress/prep');
}

const QUESTIONS = [
  '年假有几天？',
  '报销单笔超过 500 元谁签字？',
  '试用期员工有年假吗？',
  '病假需要提供什么证明？',
  '请假需要提前多久申请？',
];

// ---------------- 自定义指标 ----------------

const okCount = new Counter('deeptalk_scenario_ok');               // status_code=1000
const bizFail = new Counter('deeptalk_scenario_biz_fail');         // 其它业务码
const rateLimited = new Counter('deeptalk_scenario_rate_limited'); // 4002
const quotaExceeded = new Counter('deeptalk_scenario_quota');      // 4003
const modelFail = new Counter('deeptalk_scenario_model_fail');     // 5003 / 5004
const chatDuration = new Trend('deeptalk_chat_duration', true);    // 端到端耗时
const sseTTFB = new Trend('deeptalk_sse_ttfb', true);              // 流式首字节延迟

// ---------------- 场景定义 ----------------

function scenarioConfig() {
  const name = __ENV.SCENARIO || 'multi';

  // 同会话并发故意把所有流量压到 1 个会话上，用来量化会话锁的排队代价
  if (name === 'same') {
    return {
      name,
      options: {
        vus: parseInt(__ENV.VUS || '10', 10),
        duration: __ENV.DURATION || '1m',
      },
    };
  }

  const defaults = {
    local: { vus: 50, duration: '1m' },
    multi: { vus: 20, duration: '2m' },
    repeat: { vus: 10, duration: '1m' },
    long: { vus: 10, duration: '1m' },
    stream: { vus: 10, duration: '1m' },
    mixed: { vus: 20, duration: '2m' },
  }[name] || { vus: 20, duration: '2m' };

  return {
    name,
    options: {
      vus: parseInt(__ENV.VUS || String(defaults.vus), 10),
      duration: __ENV.DURATION || defaults.duration,
    },
  };
}

const sc = scenarioConfig();

export const options = {
  ...sc.options,
  // 业务失败率超过 1% 视为不通过；限流单独看，不算失败（压测时它可能是预期行为）
  thresholds: {
    'deeptalk_scenario_biz_fail': ['count<1'],
    'http_req_failed': ['rate<0.05'],
    'deeptalk_chat_duration': ['p(95)<30000'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

// ---------------- 请求工具 ----------------

function authHeaders(token) {
  return {
    'Content-Type': 'application/json',
    Authorization: 'Bearer ' + token,
  };
}

function pickUser() {
  // 每个 VU 绑定固定账号，保证同一个 VU 下的请求落在同一个用户上（贴近真实用户）
  return users[(__VU - 1) % users.length];
}

function pickSession(user, salt) {
  if (!user.sessionIds || user.sessionIds.length === 0) {
    throw new Error('用户 ' + user.username + ' 没有会话，请重跑 prep');
  }
  return user.sessionIds[(salt + __ITER) % user.sessionIds.length];
}

// 统一解析业务响应：DeepTalk 即使业务失败也返回 HTTP 200，必须看 status_code
function classify(res) {
  let body = {};
  try {
    body = res.json() || {};
  } catch (e) {
    body = {};
  }
  const code = body.status_code;

  if (res.status === 429 || code === 4002) {
    rateLimited.add(1);
    return 'rate_limited';
  }
  if (code === 4003) {
    quotaExceeded.add(1);
    return 'quota';
  }
  if (code === 5003 || code === 5004) {
    modelFail.add(1);
    return 'model_fail';
  }
  if (code === 1000) {
    okCount.add(1);
    return 'ok';
  }
  bizFail.add(1);
  return 'biz_fail(' + code + ')';
}

function post(path, payload, token, tag) {
  const res = http.post(ENV.baseUrl + ENV.apiPrefix + path, JSON.stringify(payload), {
    headers: authHeaders(token),
    tags: { scenario: sc.name, endpoint: tag },
  });
  chatDuration.add(res.timings.duration, { scenario: sc.name, endpoint: tag });
  return res;
}

function longQuestion() {
  const base = '请根据文档说明年假的计算方式与结转规则。';
  const filler = '（补充背景：这是一次压测请求，用于把会话历史推到压缩水位线以上，请忽略本段说明。）';
  let q = base;
  const target = Math.min(ENV.longPad, 3500);
  while (q.length < target) {
    q += filler;
  }
  return q.slice(0, target);
}

// ---------------- 各场景的执行体 ----------------

function runLocal(user, sessionId) {
  const list = http.get(ENV.baseUrl + ENV.apiPrefix + '/chat/sessions', {
    headers: authHeaders(user.token),
    tags: { scenario: sc.name, endpoint: 'sessions' },
  });
  // 必须走 classify，否则成功/失败计数器一直是 n/a（这两个接口也会返回业务码）
  classify(list);
  check(list, { 'sessions ok': (r) => r.status === 200 });
  chatDuration.add(list.timings.duration, { endpoint: 'sessions' });

  const hist = post('/chat/history', { sessionId }, user.token, 'history');
  classify(hist);
  check(hist, { 'history ok': (r) => r.status === 200 });
}

function runChat(user, sessionId, question) {
  const res = post('/chat/send', { question, sessionId, modelType: '' }, user.token, 'chat_send');
  const kind = classify(res);
  check(res, { 'http 2xx': (r) => r.status >= 200 && r.status < 300 });
  return kind;
}

function runNewSession(user, question) {
  // 每次新建会话 => 每次都是"首轮提问"，这样才会命中语义缓存（语义缓存只对首轮生效）
  const res = post(
    '/chat/send-new-session',
    { question, modelType: ENV.modelType },
    user.token,
    'chat_new_session'
  );
  classify(res);
  return res;
}

function runStream(user, sessionId, question) {
  const res = http.post(
    ENV.baseUrl + ENV.apiPrefix + '/chat/send-stream',
    JSON.stringify({ question, sessionId, modelType: '' }),
    {
      headers: authHeaders(user.token),
      tags: { scenario: sc.name, endpoint: 'chat_stream' },
      // 流式响应必须完整读完，否则连接不会释放，压测结果会失真
      timeout: '120s',
    }
  );
  // k6 里 http_req_waiting ≈ 首字节时间
  sseTTFB.add(res.timings.waiting, { scenario: sc.name });
  chatDuration.add(res.timings.duration, { scenario: sc.name, endpoint: 'chat_stream' });
  check(res, { 'stream 200': (r) => r.status === 200 });
}

// ---------------- 主循环 ----------------

export default function () {
  const user = pickUser();

  switch (sc.name) {
    case 'local': {
      runLocal(user, pickSession(user, 0));
      break;
    }

    case 'same': {
      // 所有 VU 共用第一个用户的第一个会话：专门测会话锁串行化
      const shared = users[0];
      runChat(shared, shared.sessionIds[0], QUESTIONS[__ITER % QUESTIONS.length]);
      break;
    }

    case 'multi': {
      runChat(user, pickSession(user, 0), QUESTIONS[__ITER % QUESTIONS.length]);
      break;
    }

    case 'repeat': {
      // 全程同一个问题：命中意图缓存 + 语义缓存 + 上游前缀缓存
      runNewSession(user, '年假有几天？');
      break;
    }

    case 'long': {
      runChat(user, pickSession(user, 0), longQuestion());
      break;
    }

    case 'stream': {
      runStream(user, pickSession(user, 0), QUESTIONS[__ITER % QUESTIONS.length]);
      break;
    }

    case 'mixed':
    default: {
      const roll = Math.random();
      if (roll < 0.15) {
        runLocal(user, pickSession(user, 0));
      } else if (roll < 0.25) {
        runNewSession(user, '年假有几天？');
      } else {
        runChat(user, pickSession(user, 0), QUESTIONS[__ITER % QUESTIONS.length]);
      }
      break;
    }
  }

  sleep(parseFloat(__ENV.SLEEP || '0.2'));
}

// ---------------- 结果落盘 ----------------

export function handleSummary(data) {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const path = `stress/results/${sc.name}-${stamp}.json`;
  console.log(`[k6] 场景=${sc.name} 结果写入 ${path}`);
  return {
    stdout: textSummary(data),
    [path]: JSON.stringify(data, null, 2),
  };
}

// 精简版摘要：只保留我们最关心的几个数字
function textSummary(data) {
  const m = data.metrics;
  const lines = [];
  const get = (name, field) => (m[name] && m[name].values ? m[name].values[field] : undefined);
  const fmt = (v) => (v === undefined ? 'n/a' : typeof v === 'number' ? v.toFixed(2) : v);

  lines.push('');
  lines.push('================ DeepTalk 压测摘要 ================');
  lines.push(`场景            : ${sc.name}`);
  lines.push(`并发 VU         : ${sc.options.vus}`);
  lines.push(`时长            : ${sc.options.duration}`);
  lines.push(`请求数          : ${fmt(get('http_reqs', 'count'))}`);
  lines.push(`RPS             : ${fmt(get('http_reqs', 'rate'))}`);
  lines.push('');
  lines.push(`业务成功(1000)  : ${fmt(get('deeptalk_scenario_ok', 'count'))}`);
  lines.push(`业务失败        : ${fmt(get('deeptalk_scenario_biz_fail', 'count'))}`);
  lines.push(`被限流(4002)    : ${fmt(get('deeptalk_scenario_rate_limited', 'count'))}`);
  lines.push(`配额用尽(4003)  : ${fmt(get('deeptalk_scenario_quota', 'count'))}`);
  lines.push(`模型侧失败      : ${fmt(get('deeptalk_scenario_model_fail', 'count'))}`);
  lines.push('');
  lines.push(`端到端 avg      : ${fmt(get('deeptalk_chat_duration', 'avg'))} ms`);
  lines.push(`端到端 p95      : ${fmt(get('deeptalk_chat_duration', 'p(95)'))} ms`);
  lines.push(`端到端 p99      : ${fmt(get('deeptalk_chat_duration', 'p(99)'))} ms`);
  if (m['deeptalk_sse_ttfb']) {
    lines.push(`SSE 首字节 p95  : ${fmt(get('deeptalk_sse_ttfb', 'p(95)'))} ms`);
  }
  lines.push('===================================================');
  lines.push('别忘了去看服务端 /metrics 与 pprof（block/mutex profile 才是找锁竞争的关键）');
  lines.push('');
  return lines.join('\n');
}
