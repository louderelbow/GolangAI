<template>
  <div class="page">
    <div class="particles">
      <span v-for="i in 16" :key="i" class="dot" :style="dotStyle(i)"></span>
    </div>

    <aside class="sidebar">
      <div class="sidebar-brand">
        <svg viewBox="0 0 64 64" fill="none" class="sidebar-logo">
          <rect x="8" y="20" width="14" height="24" rx="4" fill="white" opacity="0.9"/>
          <rect x="26" y="8" width="14" height="36" rx="4" fill="white" opacity="0.7"/>
          <rect x="44" y="14" width="14" height="30" rx="4" fill="white" opacity="0.5"/>
          <circle cx="16" cy="14" r="4" fill="#a78bfa"/>
          <circle cx="34" cy="4" r="3.5" fill="#c084fc"/>
          <circle cx="52" cy="9" r="3" fill="#e879f9"/>
        </svg>
        <span class="sidebar-name">DeepTalk</span>
      </div>

      <button class="new-chat-btn" @click="createNewSession">
        <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
        新聊天
      </button>

      <div class="session-label">历史会话</div>

      <ul class="session-list">
        <li
          v-for="session in sessions"
          :key="session.id"
          :class="['session-item', { active: currentSessionId === session.id }]"
          @click="switchSession(session.id)"
        >
          <span class="session-text">{{ session.name || `会话 ${session.id}` }}</span>
        </li>
      </ul>

      <div class="sidebar-footer">
        <button class="back-menu-btn" @click="$router.push('/menu')">
          <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="15 18 9 12 15 6"/></svg>
          返回菜单
        </button>
      </div>
    </aside>

    <main class="chat-area">
      <div class="topbar">
        <div class="topbar-left">
          <span class="topbar-title">AI 对话</span>
          <select
            id="modelType"
            v-model="modelSelectValue"
            class="model-select"
            :disabled="modelLocked"
            :title="modelHint"
          >
            <option v-for="opt in MODEL_OPTIONS" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
          </select>
          <span :class="['model-hint', { locked: modelLocked }]">
            <svg v-if="modelLocked" viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0110 0v4"/></svg>
            {{ modelHint }}
          </span>
        </div>
        <div class="topbar-right">
          <label class="stream-label">
            <input type="checkbox" v-model="isStreaming" />
            流式响应
          </label>
          <!-- 本地工作区：连接状态一目了然，点开就能换目录。
               刻意放在顶栏而不是藏进设置页——它是"模型能不能碰你硬盘"的开关，
               用户必须随时看得见当前状态。 -->
          <button class="tool-btn ws-chip" :class="{ on: ws.connected }" @click="openWorkspace">
            <span class="ws-dot"></span>
            {{ ws.connected ? '本机 · 已连接' : '连接本机文件' }}
          </button>
          <button class="tool-btn" @click="syncHistory" :disabled="!currentSessionId || tempSession">
            <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polyline points="23 4 23 10 17 10"/><path d="M20.49 15a9 9 0 11-2.12-9.36L23 10"/></svg>
            同步
          </button>
          <button class="tool-btn upload" @click="triggerFileUpload" :disabled="uploading">
            <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/></svg>
            {{ uploading ? '上传中' : '文档' }}
          </button>
          <input ref="fileInput" type="file" accept=".md,.txt,text/markdown,text/plain" style="display:none" @change="handleFileUpload" />
        </div>
      </div>

      <div class="messages" ref="messagesRef">
        <div
          v-for="(message, index) in currentMessages"
          v-show="!message.hidden"
          :key="index"
          :class="['bubble', message.role === 'user' ? 'bubble-user' : 'bubble-ai']"
        >
          <div class="bubble-meta">
            <span class="bubble-role">{{ message.role === 'user' ? '你' : 'AI' }}</span>
            <span v-if="message.meta && message.meta.status === 'streaming'" class="streaming-dot"></span>
          </div>
          <div class="bubble-content" v-html="renderMarkdown(message.content)"></div>
        </div>
      </div>

      <div class="input-bar">
        <textarea
          v-model="inputMessage"
          placeholder="输入你的问题..."
          @keydown.enter.exact.prevent="sendMessage"
          :disabled="loading"
          ref="messageInput"
          rows="1"
          class="chat-textarea"
        ></textarea>
        <button
          type="button"
          :disabled="!inputMessage.trim() || loading"
          @click="sendMessage"
          class="send-btn"
        >
          <svg v-if="!loading" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
          <span v-else>...</span>
        </button>
      </div>
    </main>

    <!-- 澄清追问弹窗：Agent 认为信息不足时弹出，必须点选一项才能继续。
         刻意不进消息流——提问不是"AI 说了句话"，而是"这一轮缺一个输入"，
         混在气泡里会被当成普通回复划过去，用户不知道对话正卡在这里。 -->
    <div v-if="clarifyPrompt" class="clarify-mask">
      <div class="clarify-modal" role="dialog" aria-modal="true" aria-labelledby="clarify-q">
        <div class="clarify-head">
          <span class="clarify-tag">需要你确认</span>
          <span class="clarify-count">按数字键 1–{{ clarifyPrompt.options.length }} 快速选择</span>
        </div>

        <div class="clarify-question" id="clarify-q">{{ clarifyPrompt.question }}</div>
        <div v-if="clarifyPrompt.reason" class="clarify-reason">{{ clarifyPrompt.reason }}</div>

        <div class="clarify-options">
          <button
            v-for="(opt, i) in clarifyPrompt.options"
            :key="opt.id || i"
            type="button"
            class="clarify-option"
            :disabled="loading"
            @click="answerClarify(opt)"
          >
            <span class="clarify-index">{{ i + 1 }}</span>
            <span class="clarify-option-text">
              <span class="clarify-option-label">{{ opt.label }}</span>
              <span v-if="opt.hint" class="clarify-option-hint">{{ opt.hint }}</span>
            </span>
          </button>
        </div>

        <!-- 「其他」：选项是模型列的，未必覆盖用户的情况。
             没有这一栏时，用户只能二选一或关掉重来，比不弹还难受。 -->
        <div class="clarify-other">
          <input
            v-model="clarifyOther"
            type="text"
            class="clarify-other-input"
            maxlength="200"
            placeholder="都不是？直接说明你的情况"
            :disabled="loading"
            @keydown.enter.prevent="submitClarifyOther"
          />
          <button
            type="button"
            class="clarify-other-btn"
            :disabled="!clarifyOther.trim() || loading"
            @click="submitClarifyOther"
          >确定</button>
        </div>

        <div class="clarify-foot">
          <span v-if="loading" class="clarify-loading">已提交，正在继续…</span>
          <span v-else class="clarify-tip">选一个，或在上面直接输入</span>
        </div>
      </div>
    </div>

    <!-- 本地工作区面板：连接状态 + 换目录。
         工作区由用户**在自己电脑上**通过系统选择框决定，服务端只能提请——
         否则被攻破的服务端可以先把工作区改成 C:\ 再读光整块盘。 -->
    <div v-if="wsPanel" class="ws-mask" @click.self="wsPanel = false">
      <div class="ws-modal" role="dialog" aria-modal="true">
        <div class="ws-head">
          <span class="ws-tag">本机文件访问</span>
          <button class="ws-close" type="button" @click="wsPanel = false">×</button>
        </div>

        <div v-if="ws.enabled === false" class="ws-state warn">
          服务端没有开启这个能力（<code>[localAgent] enabled = false</code>）。<br />
          需要部署者在 <code>config/config.toml</code> 里打开并重启服务端。
        </div>

        <template v-else-if="ws.connected">
          <div class="ws-state ok">已连接 · 只读</div>
          <div class="ws-path">{{ ws.workspace }}</div>
          <div class="ws-tip">模型可以列出并读取这个目录里的文件；越界路径由你的电脑拒绝。</div>
          <div class="ws-actions">
            <button class="ws-btn primary" type="button" :disabled="wsBusy" @click="changeWorkspace">
              {{ wsBusy ? '等待你在本机选择…' : '更换目录' }}
            </button>
            <button class="ws-btn" type="button" :disabled="wsBusy" @click="refreshWorkspace">重新检测</button>
          </div>
          <div class="ws-hint">点「更换目录」会在<b>你自己的电脑上</b>弹出系统目录选择框。</div>
        </template>

        <template v-else>
          <div class="ws-state warn">未连接</div>
          <div class="ws-tip">
            本机连接器没有在运行。它必须在<b>你自己的电脑</b>上启动——网页没法替你起一个本机进程。
            但只需要设置一次：
          </div>
          <ol class="ws-steps">
            <li>在项目根目录构建一次：<code>go build -o deeptalk-agent.exe ./cmd/localagent</code></li>
            <li>首次运行时给它一次登录 token（服务端、token、工作区会被一起记住）：
              <pre>set DEEPTALK_TOKEN=&lt;你在本页的登录 token&gt;
deeptalk-agent.exe</pre>
            </li>
            <li>之后<b>每次直接运行 deeptalk-agent.exe 就行</b>，它自己连上、自己用上次的工作区</li>
          </ol>
          <div class="ws-actions">
            <button class="ws-btn primary" type="button" :disabled="wsBusy" @click="refreshWorkspace">
              {{ wsBusy ? '检测中…' : '我启动了，重新检测' }}
            </button>
            <button class="ws-btn" type="button" @click="copyToken">复制我的 token</button>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<script>
import { ref, nextTick, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import api from '../utils/api'

export default {
  name: 'AIChat',
  setup() {
    const sessions = ref({})
    const currentSessionId = ref(null)
    const tempSession = ref(false)
    const currentMessages = ref([])
    const inputMessage = ref('')
    const loading = ref(false)
    const messagesRef = ref(null)
    const messageInput = ref(null)
    const selectedModel = ref('2')   // 仅用于「新会话」的模型选择
    const isStreaming = ref(false)
    const uploading = ref(false)
    const fileInput = ref(null)

    // 待回答的澄清提问（null 表示当前没有弹窗）。
    // 服务端不保存"未完成的提问"这类状态，所以原问题 ask 存在这里，
    // 用户选完后再随 clarifyAnswer 一起回传。
    const clarifyPrompt = ref(null)
    // 弹窗里"其他"那一栏的输入。选项是模型列的，未必覆盖用户的情况。
    const clarifyOther = ref('')

    // 本地工作区状态（来自服务端 /agent/status）。
    // 轮询是为了"用户刚在本机启动了连接器"这件事能自动反映到界面，
    // 而不用手动刷新整个页面——那是终端思维，不该让用户做。
    const ws = ref({ connected: false, enabled: null, workspace: '' })
    const wsPanel = ref(false)
    const wsBusy = ref(false)
    let wsTimer = null

    // 只剩两条路径：RAG（必然查文档）与 Unified Agent（自主决定用不用工具）。
    // 原 modelType 1（DeepSeek 纯对话）已删除，历史会话由服务端映射到 6。
    const MODEL_OPTIONS = [
      { value: '2', label: '阿里百炼 RAG' },
      { value: '6', label: 'Unified Agent' }
    ]

    // 记住上次打开的会话：刷新页面后自动恢复，避免"记录看起来没了"
    const LAST_SESSION_KEY = 'deeptalk:lastSessionId'

    const modelLabel = (value) => {
      const hit = MODEL_OPTIONS.find(o => o.value === String(value || ''))
      return hit ? hit.label : '未知模型'
    }

    // 当前会话实际绑定的模型：新会话取下拉框选择，已有会话取服务端返回的绑定值
    const activeModel = computed(() => {
      if (tempSession.value) return selectedModel.value
      const current = sessions.value[currentSessionId.value]
      return current && current.modelType ? String(current.modelType) : ''
    })

    // 会话的模型创建后不可更改：非新会话时下拉框禁用、只展示
    const modelLocked = computed(() => !tempSession.value)

    const modelSelectValue = computed({
      get: () => (tempSession.value ? selectedModel.value : activeModel.value),
      set: (value) => {
        if (tempSession.value) selectedModel.value = value
      }
    })

    const modelHint = computed(() => {
      if (tempSession.value) return '新会话 · 可选择模型'
      const model = activeModel.value
      return model ? `当前会话模型：${modelLabel(model)}（不可更改）` : '当前会话模型：未知（不可更改）'
    })

    const dotStyle = (i) => {
      const size = 1.5 + (i % 3) * 1.5
      return {
        width: size + 'px',
        height: size + 'px',
        left: ((i * 37 + 13) % 100) + '%',
        top: ((i * 53 + 7) % 100) + '%',
        animationDelay: (i * 0.7) + 's',
        animationDuration: (4 + (i % 5)) + 's',
        opacity: 0.06 + (i % 3) * 0.04
      }
    }

    const escapeHtml = (text) => String(text)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;')

    // 先转义再套轻量 markdown：模型输出（含被检索文档诱导的内容）不能作为 HTML 执行
    const renderMarkdown = (text) => {
      if (!text && text !== '') return ''
      return escapeHtml(text)
        .replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>')
        .replace(/\*(.*?)\*/g, '<em>$1</em>')
        .replace(/`(.*?)`/g, '<code>$1</code>')
        .replace(/\n/g, '<br>')
    }

    const loadSessions = async () => {
      try {
        const response = await api.get('/AI/chat/sessions')
        if (response.data && response.data.status_code === 1000 && Array.isArray(response.data.sessions)) {
          const sessionMap = {}
          response.data.sessions.forEach(s => {
            const sid = String(s.sessionId)
            sessionMap[sid] = {
              id: sid,
              name: s.name || `会话 ${sid}`,
              modelType: s.modelType ? String(s.modelType) : '', // 服务端返回的会话绑定模型
              messages: []
            }
          })
          sessions.value = sessionMap
        }
      } catch (error) {
        console.error('Load sessions error:', error)
      }
    }

    const createNewSession = () => {
      currentSessionId.value = 'temp'
      tempSession.value = true
      currentMessages.value = []
      clarifyPrompt.value = null // 切走会话时收起未答的追问，避免答到别的会话上
      localStorage.removeItem(LAST_SESSION_KEY)
      nextTick(() => {
        if (messageInput.value) messageInput.value.focus()
      })
    }

    const switchSession = async (sessionId) => {
      if (!sessionId) return
      currentSessionId.value = String(sessionId)
      tempSession.value = false
      clarifyPrompt.value = null // 切走会话时收起未答的追问，避免答到别的会话上
      localStorage.setItem(LAST_SESSION_KEY, currentSessionId.value)

      if (!sessions.value[sessionId].messages || sessions.value[sessionId].messages.length === 0) {
        try {
          const response = await api.post('/AI/chat/history', { sessionId: currentSessionId.value })
          if (response.data && response.data.status_code === 1000 && Array.isArray(response.data.history)) {
            const messages = response.data.history.map(item => ({
              role: item.is_user ? 'user' : 'assistant',
              content: item.content
            }))
            sessions.value[sessionId].messages = messages
          }
        } catch (err) {
          console.error('Load history error:', err)
        }
      }
      currentMessages.value = [...(sessions.value[sessionId].messages || [])]
      await nextTick()
      scrollToBottom()
    }

    const syncHistory = async () => {
      if (!currentSessionId.value || tempSession.value) {
        ElMessage.warning('请选择已有会话进行同步')
        return
      }
      try {
        const response = await api.post('/AI/chat/history', { sessionId: currentSessionId.value })
        if (response.data && response.data.status_code === 1000 && Array.isArray(response.data.history)) {
          const messages = response.data.history.map(item => ({
            role: item.is_user ? 'user' : 'assistant',
            content: item.content
          }))
          sessions.value[currentSessionId.value].messages = messages
          currentMessages.value = [...messages]
          await nextTick()
          scrollToBottom()
        } else {
          ElMessage.error('无法获取历史数据')
        }
      } catch (err) {
        console.error('Sync history error:', err)
        ElMessage.error('请求历史数据失败')
      }
    }

    const sendMessage = async () => {
      if (!inputMessage.value || !inputMessage.value.trim()) {
        ElMessage.warning('请输入消息内容')
        return
      }
      const text = inputMessage.value
      inputMessage.value = ''
      await submitMessage(text, null)
    }

    // appendClarifyAnswer 把用户的回应拼进原问题，用于**显示**。
    //
    // 格式刻意与后端 service/session/clarify.go 的 augmentQuestion 一致：
    // 界面看到的和真正进上下文的那段文字相同，用户对照历史时才不会困惑。
    const appendClarifyAnswer = (prompt, answer, custom) => {
      const verb = custom ? '用户补充说明' : '用户选择了'
      const base = prompt.ask || ''
      if (!base) return `（补充信息：${verb}「${answer}」）`
      return `${base}\n（补充信息：针对「${prompt.question}」，${verb}「${answer}」）`
    }

    // answerClarify 用户回应了弹窗：把选择随**原问题**一起发回去。
    //
    // 服务端不保存"未完成的提问"这类会话状态，靠前端把原问题和选择一起带回，
    // 因此刷新页面、换实例部署都不会把这个选择弄丢。
    const answerClarify = async (opt, custom) => {
      const prompt = clarifyPrompt.value
      if (!prompt || loading.value) return

      const answer = String(opt.label || '').trim()
      if (!answer) return

      clarifyPrompt.value = null // 先收起弹窗：防止等待响应期间重复点击
      clarifyOther.value = ''

      const payload = { question: prompt.question, label: answer }
      // 自己填的要标出来：后端据此把"选择了"改成"补充说明"，
      // 免得模型以为用户认可了预设的那几个选项。
      if (custom) payload.custom = true

      await submitMessage(prompt.ask || '', payload, appendClarifyAnswer(prompt, answer, custom))
    }

    // submitClarifyOther 用户没点选项，自己填了一个。
    const submitClarifyOther = () => {
      const text = clarifyOther.value.trim()
      if (!text || loading.value) return
      answerClarify({ label: text }, true)
    }

    // 数字键快速选择。挂在 window 上而不是弹窗元素上：
    // 焦点可能落在页面的任何地方（用户点了遮罩、按了 Tab），
    // 挂在元素上时按键会静默丢失。
    const onClarifyKeydown = (e) => {
      const prompt = clarifyPrompt.value
      if (!prompt || loading.value) return

      // 焦点在"其他"输入框里时，数字就是数字——不能当成快捷键抢走，
      // 否则用户永远打不出带数字的回答。
      const target = e.target
      const tag = (target && target.tagName) || ''
      if (tag === 'INPUT' || tag === 'TEXTAREA' || (target && target.isContentEditable)) return

      // 不给 Esc 关闭：这一轮必须补上信息才能继续，关掉就只剩一个空回答
      const n = Number.parseInt(e.key, 10)
      if (Number.isInteger(n) && n >= 1 && n <= prompt.options.length) {
        e.preventDefault()
        answerClarify(prompt.options[n - 1])
      }
    }

    const submitMessage = async (text, clarifyAnswer, displayText) => {
      const userMessage = { role: 'user', content: displayText || text }

      currentMessages.value.push(userMessage)
      await nextTick()
      scrollToBottom()

      try {
        loading.value = true
        if (isStreaming.value) {
          await handleStreaming(text, clarifyAnswer)
        } else {
          await handleNormal(text, clarifyAnswer, displayText)
        }
      } catch (err) {
        console.error('Send message error:', err)
        ElMessage.error('发送失败，请重试（如果是初次打开页面需要先创建新对话）')
        if (!tempSession.value && currentSessionId.value && sessions.value[currentSessionId.value] && sessions.value[currentSessionId.value].messages) {
          const sessionArr = sessions.value[currentSessionId.value].messages
          if (sessionArr && sessionArr.length) sessionArr.pop()
        }
        currentMessages.value.pop()
      } finally {
        if (!isStreaming.value) loading.value = false
        await nextTick()
        scrollToBottom()
      }
    }

    // ==================== 本地工作区 ====================

    // refreshWorkspace 拉一次连接状态。失败就当作"未连接"——
    // 状态面板报错比"连不上"更难懂。
    async function refreshWorkspace() {
      try {
        const res = await api.get('/agent/status')
        ws.value = res.data || { connected: false }
      } catch (e) {
        ws.value = { connected: false }
      }
    }

    const openWorkspace = async () => {
      wsPanel.value = true
      await refreshWorkspace()
    }

    // changeWorkspace 请求本机弹出目录选择框。
    //
    // 传过去的 path 只是**选择框的初始位置**——最终选哪个目录由用户在你自己的
    // 电脑上点。工作区是本地连接器能触碰的边界，这个决定权不能交给服务端。
    const changeWorkspace = async () => {
      wsBusy.value = true
      try {
        const res = await api.post('/agent/workspace', { path: ws.value.workspace || '' })
        if (res.data && res.data.error) {
          ElMessage.warning(res.data.error)
        } else {
          ws.value = { ...ws.value, ...res.data }
          ElMessage.success('工作区已切换')
        }
      } catch (e) {
        ElMessage.error('请求失败，请确认本机连接器还在运行')
      } finally {
        wsBusy.value = false
      }
    }

    // copyToken 把当前登录 token 复制出来，省得用户去翻浏览器存储。
    // 只在本机连接器首次配置时用一次。
    const copyToken = async () => {
      const token = localStorage.getItem('token') || ''
      if (!token) {
        ElMessage.warning('本地没有登录 token，请重新登录')
        return
      }
      try {
        await navigator.clipboard.writeText(token)
        ElMessage.success('token 已复制，粘贴到本机连接器那一步即可')
      } catch (e) {
        ElMessage.warning('浏览器拒绝了剪贴板访问，请在控制台执行 localStorage.getItem("token") 手动获取')
      }
    }

    // showWarnings 展示本轮的工具告警。    //
    // 刻意用 warning 而不是 error：工具失败已经被后端回填成 observation，
    // 本轮照常有回答，只是内容可能不完整。渲染成"失败"会让用户以为白问了。
    function showWarnings(list) {
      if (!Array.isArray(list)) return
      list.forEach((msg) => {
        if (msg) ElMessage.warning(String(msg))
      })
    }

    // pushClarify 把 Agent 的提问变成**弹窗**，而不是聊天流里的一条消息。
    function pushClarify(payload, ask) {
      const options = Array.isArray(payload.options) ? payload.options.filter(o => o && o.label) : []
      if (!payload.question || options.length === 0) {
        // 没有可选项的"提问"没法弹窗，退化成一条提示：
        // 至少让用户知道模型在等他补充信息，而不是对着空回答发呆
        ElMessage.warning(String(payload.question || '模型需要你补充信息，但没有给出可选项'))
        return
      }
      clarifyPrompt.value = {
        question: String(payload.question),
        reason: payload.reason || '',
        options,
        ask // 原问题：用户选完后要带着它一起回传
      }
      clarifyOther.value = '' // 上一轮的输入不能带到这一轮
      // 把焦点从输入框挪开：否则数字键会被 textarea 吃掉，
      // 快捷键看起来"没反应"，实际是输入框在接收字符
      nextTick(() => {
        if (document.activeElement instanceof HTMLElement) document.activeElement.blur()
      })
    }

    async function handleStreaming(question, clarifyAnswer) {
      const aiMessage = { role: 'assistant', content: '', meta: { status: 'streaming' } }
      const aiMessageIndex = currentMessages.value.length
      currentMessages.value.push(aiMessage)

      if (!tempSession.value && currentSessionId.value && sessions.value[currentSessionId.value]) {
        if (!sessions.value[currentSessionId.value].messages) sessions.value[currentSessionId.value].messages = []
        sessions.value[currentSessionId.value].messages.push({ role: 'assistant', content: '' })
      }

      const url = tempSession.value ? '/api/AI/chat/send-stream-new-session' : '/api/AI/chat/send-stream'
      const headers = { 'Content-Type': 'application/json', 'Authorization': `Bearer ${localStorage.getItem('token') || ''}` }
      const requestModel = activeModel.value || selectedModel.value
      const body = tempSession.value
        ? { question: question, modelType: requestModel }
        : { question: question, modelType: requestModel, sessionId: currentSessionId.value }
      if (clarifyAnswer) body.clarifyAnswer = clarifyAnswer

      let streamError = ''

      try {
        const response = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body) })
        if (!response.ok) { loading.value = false; throw new Error('Network response was not ok') }

        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        const appendContent = (text) => {
          if (!text) return
          currentMessages.value[aiMessageIndex].content += text
          currentMessages.value = [...currentMessages.value]
          scrollToBottom()
        }

        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          const chunk = decoder.decode(value, { stream: true })
          buffer += chunk
          const lines = buffer.split('\n')
          buffer = lines.pop() || ''

          for (const line of lines) {
            const trimmedLine = line.trim()
            if (!trimmedLine || !trimmedLine.startsWith('data:')) continue
            const data = trimmedLine.slice(5).trim()
            if (!data) continue

            if (data === '[DONE]') {
              loading.value = false
              currentMessages.value[aiMessageIndex].meta = { status: 'done' }
              currentMessages.value = [...currentMessages.value]
              continue
            }

            // 新协议：所有事件都是 JSON（content / sessionId+modelType / clarify / error）
            let payload = null
            if (data.startsWith('{')) {
              try { payload = JSON.parse(data) } catch (e) { payload = null }
            }

            if (payload && typeof payload === 'object') {
              if (payload.error) {
                streamError = String(payload.error)
                continue
              }
              if (payload.type === 'warning') {
                // 工具没取到数据：本轮照常结束，只提示一句
                showWarnings([payload.message])
                continue
              }
              if (payload.type === 'clarify') {
                // 这轮不是回答，而是提问：弹窗负责交互，消息流里不该留东西。
                // 若同时流出了半截正文（模型先说了半句再决定追问），保留它；
                // 否则把占位的空气泡标成 hidden，而不是塞进一个"提问气泡"。
                const placeholder = currentMessages.value[aiMessageIndex]
                if (placeholder && placeholder.content) {
                  placeholder.meta = { status: 'done' }
                } else if (placeholder) {
                  placeholder.hidden = true
                  placeholder.content = ''
                  currentMessages.value = [...currentMessages.value]
                }
                pushClarify(payload, question)
                streamError = ''
                continue
              }
              if (payload.sessionId) {
                const newSid = String(payload.sessionId)
                if (tempSession.value) {
                  sessions.value[newSid] = {
                    id: newSid,
                    name: '新会话',
                    modelType: String(payload.modelType || requestModel), // 服务端确认的绑定模型
                    messages: [...currentMessages.value]
                  }
                  currentSessionId.value = newSid
                  tempSession.value = false
                  localStorage.setItem(LAST_SESSION_KEY, newSid)
                }
                continue
              }
              if (typeof payload.content === 'string') {
                appendContent(payload.content)
                continue
              }
              continue
            }

            // 兼容旧后端的纯文本分片
            appendContent(data)
          }
        }
        await new Promise(resolve => requestAnimationFrame(resolve))

        loading.value = false
        const tail = currentMessages.value[aiMessageIndex]
        if (tail) tail.meta = { status: streamError ? 'error' : 'done' }
        currentMessages.value = [...currentMessages.value]

        if (streamError) {
          ElMessage.error(streamError)
        }

        if (!tempSession.value && currentSessionId.value && sessions.value[currentSessionId.value]) {
          const sessMsgs = sessions.value[currentSessionId.value].messages
          if (Array.isArray(sessMsgs) && sessMsgs.length) {
            const lastIndex = sessMsgs.length - 1
            if (sessMsgs[lastIndex] && sessMsgs[lastIndex].role === 'assistant') {
              if (tail && tail.hidden) {
                // 本轮是提问、没有正文：把会话里那份占位也去掉，
                // 否则切走再切回来会看到一个空气泡
                sessMsgs.splice(lastIndex, 1)
              } else {
                sessMsgs[lastIndex].content = tail ? tail.content : ''
              }
            }
          }
        }
      } catch (err) {
        console.error('Stream error:', err)
        loading.value = false
        const tail = currentMessages.value[aiMessageIndex]
        if (tail) tail.meta = { status: 'error' }
        currentMessages.value = [...currentMessages.value]
        ElMessage.error('流式传输出错')
      }
    }

    async function handleNormal(question, clarifyAnswer, displayText) {
      // 发出去的永远是原问题；displayText 只影响消息流里显示成什么
      // （澄清那一轮要带上用户选了什么，否则会出现两条一样的用户消息）
      const shownQuestion = displayText || question
      if (tempSession.value) {
        const body = { question, modelType: selectedModel.value }
        if (clarifyAnswer) body.clarifyAnswer = clarifyAnswer
        const response = await api.post('/AI/chat/send-new-session', body)
        // 工具告警在澄清/成功两条路径上都可能带
        showWarnings(response.data && response.data.warnings)

        // 澄清（3001）：新会话第一轮就可能反问用户。
        // 会话已经建好了，所以要照常切过去，只是改由弹窗来问。
        if (response.data && response.data.status_code === 3001) {
          const sessionId = String(response.data.sessionId || '')
          if (sessionId) {
            sessions.value[sessionId] = {
              id: sessionId,
              name: '新会话',
              modelType: String(response.data.modelType || selectedModel.value),
              messages: [{ role: 'user', content: shownQuestion }]
            }
            currentSessionId.value = sessionId
            tempSession.value = false
            localStorage.setItem(LAST_SESSION_KEY, sessionId)
            currentMessages.value = [{ role: 'user', content: shownQuestion }]
          }
          pushClarify(response.data.clarify || {}, question)
          return
        }

        if (response.data && response.data.status_code === 1000) {
          const sessionId = String(response.data.sessionId)
          const modelType = String(response.data.modelType || selectedModel.value)
          const aiMessage = { role: 'assistant', content: response.data.Information || '' }
          sessions.value[sessionId] = {
            id: sessionId,
            name: '新会话',
            modelType,
            messages: [{ role: 'user', content: shownQuestion }, aiMessage]
          }
          currentSessionId.value = sessionId
          tempSession.value = false
          localStorage.setItem(LAST_SESSION_KEY, sessionId)
          currentMessages.value = [...sessions.value[sessionId].messages]
        } else {
          ElMessage.error(response.data?.status_msg || '发送失败')
          currentMessages.value.pop()
        }
      } else {
        const sessionMsgs = sessions.value[currentSessionId.value].messages
        sessionMsgs.push({ role: 'user', content: shownQuestion })
        // 已有会话的模型由服务端按会话绑定决定，这里传值仅用于兼容
        const body = { question, modelType: activeModel.value, sessionId: currentSessionId.value }
        if (clarifyAnswer) body.clarifyAnswer = clarifyAnswer
        const response = await api.post('/AI/chat/send', body)
        showWarnings(response.data && response.data.warnings)

        // 澄清：本轮没有答案，少推一条助手消息，改推选择框
        if (response.data && response.data.status_code === 3001) {
          currentMessages.value = [...sessionMsgs]
          pushClarify(response.data.clarify || {}, question)
          return
        }

        if (response.data && response.data.status_code === 1000) {
          const aiMessage = { role: 'assistant', content: response.data.Information || '' }
          sessionMsgs.push(aiMessage)
          currentMessages.value = [...sessionMsgs]
        } else {
          ElMessage.error(response.data?.status_msg || '发送失败')
          sessionMsgs.pop()
          currentMessages.value.pop()
        }
      }
    }

    const scrollToBottom = () => {
      if (messagesRef.value) {
        try { messagesRef.value.scrollTop = messagesRef.value.scrollHeight } catch (e) { /* scroll not available */ }
      }
    }

    const triggerFileUpload = () => {
      if (fileInput.value) fileInput.value.click()
    }

    const handleFileUpload = async (event) => {
      const file = event.target.files[0]
      if (!file) return
      const fileName = file.name.toLowerCase()
      if (!fileName.endsWith('.md') && !fileName.endsWith('.txt')) {
        ElMessage.error('只允许上传 .md 或 .txt 文件')
        if (fileInput.value) fileInput.value.value = ''
        return
      }
      try {
        uploading.value = true
        const formData = new FormData()
        formData.append('file', file)
        const response = await api.post('/file/upload', formData, { headers: { 'Content-Type': 'multipart/form-data' } })
        if (response.data && response.data.status_code === 1000) {
          ElMessage.success('文件上传成功')
        } else {
          ElMessage.error(response.data?.status_msg || '上传失败')
        }
      } catch (error) {
        console.error('File upload error:', error)
        ElMessage.error('文件上传失败')
      } finally {
        uploading.value = false
        if (fileInput.value) fileInput.value.value = ''
      }
    }

    onMounted(async () => {
      window.addEventListener('keydown', onClarifyKeydown)
      await loadSessions()
      // 刷新后自动恢复上次打开的会话（并拉取它的历史）
      const last = localStorage.getItem(LAST_SESSION_KEY)
      if (last && sessions.value[last]) {
        await switchSession(last)
      }
      // 本机连接器可能在你打开页面之后才启动，所以周期性地问一句状态
      refreshWorkspace()
      wsTimer = setInterval(refreshWorkspace, 10000)
    })

    onUnmounted(() => {
      window.removeEventListener('keydown', onClarifyKeydown)
      if (wsTimer) clearInterval(wsTimer)
    })

    return {
      sessions: computed(() => Object.values(sessions.value)),
      currentSessionId, tempSession, currentMessages, inputMessage, loading,
      messagesRef, messageInput, selectedModel, isStreaming, uploading, fileInput,
      clarifyPrompt, clarifyOther,
      ws, wsPanel, wsBusy, openWorkspace, changeWorkspace, refreshWorkspace, copyToken,
      MODEL_OPTIONS, modelSelectValue, modelLocked, modelHint,
      dotStyle, renderMarkdown, createNewSession, switchSession, syncHistory,
      sendMessage, answerClarify, submitClarifyOther, triggerFileUpload, handleFileUpload
    }
  }
}
</script>

<style scoped>
.page {
  height: 100vh;
  display: flex;
  background: #0f0f1a;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial;
  overflow: hidden;
  position: relative;
}

.particles {
  position: absolute;
  inset: 0;
  pointer-events: none;
  z-index: 0;
}

.dot {
  position: absolute;
  border-radius: 50%;
  background: white;
  animation: drift linear infinite;
}

@keyframes drift {
  0%, 100% { transform: translate(0, 0); }
  25%  { transform: translate(8px, -12px); }
  50%  { transform: translate(-4px, -6px); }
  75%  { transform: translate(-10px, 4px); }
}

/* ==================== Sidebar ==================== */

.sidebar {
  width: 260px;
  background: rgba(22, 22, 40, 0.95);
  backdrop-filter: blur(24px);
  border-right: 1px solid rgba(255, 255, 255, 0.05);
  display: flex;
  flex-direction: column;
  position: relative;
  z-index: 2;
  flex-shrink: 0;
}

.sidebar-brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 20px 20px 16px;
}

.sidebar-logo {
  width: 30px;
  height: 30px;
}

.sidebar-name {
  color: white;
  font-size: 17px;
  font-weight: 700;
  letter-spacing: -0.3px;
}

.new-chat-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  margin: 0 16px 16px;
  padding: 11px 0;
  border-radius: 12px;
  border: 1px solid rgba(124, 58, 237, 0.3);
  background: rgba(124, 58, 237, 0.1);
  color: #a78bfa;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  transition: all 0.25s;
}

.new-chat-btn:hover {
  background: rgba(124, 58, 237, 0.2);
  border-color: rgba(124, 58, 237, 0.5);
  transform: translateY(-1px);
  box-shadow: 0 4px 16px rgba(124, 58, 237, 0.15);
}

.session-label {
  padding: 0 20px 10px;
  color: rgba(255, 255, 255, 0.3);
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 1.5px;
}

.session-list {
  flex: 1;
  list-style: none;
  margin: 0;
  padding: 0 10px;
  overflow-y: auto;
  min-height: 0;
}

.session-list::-webkit-scrollbar { width: 4px; }
.session-list::-webkit-scrollbar-thumb { background: rgba(255,255,255,0.06); border-radius: 4px; }
.session-list::-webkit-scrollbar-track { background: transparent; }

.session-item {
  padding: 11px 14px;
  margin-bottom: 2px;
  border-radius: 10px;
  cursor: pointer;
  color: rgba(255, 255, 255, 0.5);
  font-size: 13px;
  transition: all 0.2s;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.session-item:hover {
  background: rgba(255, 255, 255, 0.04);
  color: rgba(255, 255, 255, 0.75);
}

.session-item.active {
  background: rgba(124, 58, 237, 0.15);
  color: #c4b5fd;
  font-weight: 500;
}

.sidebar-footer {
  padding: 14px 16px;
  border-top: 1px solid rgba(255, 255, 255, 0.05);
}

.back-menu-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 10px;
  border-radius: 10px;
  border: none;
  background: rgba(255, 255, 255, 0.03);
  color: rgba(255, 255, 255, 0.4);
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  transition: all 0.2s;
}

.back-menu-btn:hover {
  background: rgba(255, 255, 255, 0.06);
  color: rgba(255, 255, 255, 0.7);
}

/* ==================== Chat Area ==================== */

.chat-area {
  flex: 1;
  display: flex;
  flex-direction: column;
  position: relative;
  z-index: 1;
  min-width: 0;
  min-height: 0;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 24px;
  background: rgba(22, 22, 40, 0.7);
  backdrop-filter: blur(20px);
  border-bottom: 1px solid rgba(255, 255, 255, 0.05);
  z-index: 3;
}

.topbar-left {
  display: flex;
  align-items: center;
  gap: 16px;
}

.topbar-title {
  color: white;
  font-size: 15px;
  font-weight: 600;
}

.model-select {
  padding: 7px 12px;
  border-radius: 8px;
  border: 1px solid rgba(255, 255, 255, 0.1);
  background: rgba(255, 255, 255, 0.04);
  color: rgba(255, 255, 255, 0.7);
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  outline: none;
  transition: all 0.2s;
}

.model-select:focus {
  border-color: #7c3aed;
  box-shadow: 0 0 0 2px rgba(124, 58, 237, 0.15);
}

.model-select option {
  background: #1a1a2e;
  color: white;
}

.model-select:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.model-hint {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 11px;
  color: rgba(255, 255, 255, 0.32);
  white-space: nowrap;
}

.model-hint.locked {
  color: rgba(167, 139, 250, 0.75);
}

.topbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
}

.stream-label {
  display: flex;
  align-items: center;
  gap: 6px;
  color: rgba(255, 255, 255, 0.5);
  font-size: 12px;
  cursor: pointer;
  user-select: none;
}

.stream-label input {
  accent-color: #7c3aed;
}

.tool-btn {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 7px 14px;
  border-radius: 8px;
  border: 1px solid rgba(255, 255, 255, 0.08);
  background: rgba(255, 255, 255, 0.03);
  color: rgba(255, 255, 255, 0.5);
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: all 0.2s;
}

.tool-btn:hover:not(:disabled) {
  background: rgba(255, 255, 255, 0.06);
  color: rgba(255, 255, 255, 0.75);
}

.tool-btn:disabled {
  opacity: 0.3;
  cursor: not-allowed;
}

.tool-btn.upload:hover:not(:disabled) {
  background: rgba(168, 85, 247, 0.1);
  border-color: rgba(168, 85, 247, 0.25);
  color: #c084fc;
}

/* ==================== Messages ==================== */

.messages {
  flex: 1;
  overflow-y: auto;
  padding: 28px 32px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.messages::-webkit-scrollbar { width: 6px; }
.messages::-webkit-scrollbar-thumb { background: rgba(255,255,255,0.06); border-radius: 6px; }
.messages::-webkit-scrollbar-track { background: transparent; }

.bubble {
  max-width: 70%;
  padding: 14px 18px;
  border-radius: 16px;
  line-height: 1.6;
  word-wrap: break-word;
  font-size: 14px;
  animation: bubbleIn 0.25s ease-out;
}

@keyframes bubbleIn {
  from { opacity: 0; transform: translateY(10px) scale(0.97); }
  to   { opacity: 1; transform: translateY(0) scale(1); }
}

.bubble-user {
  align-self: flex-end;
  background: linear-gradient(135deg, #7c3aed, #a855f7);
  color: white;
  border-bottom-right-radius: 6px;
}

.bubble-ai {
  align-self: flex-start;
  background: rgba(26, 26, 46, 0.8);
  border: 1px solid rgba(255, 255, 255, 0.06);
  color: rgba(255, 255, 255, 0.85);
  border-bottom-left-radius: 6px;
}

/* ---- 澄清追问弹窗（Agent 主动追问）----
   刻意不做成消息流里的气泡：提问是"这一轮卡住了、等你补一个输入"，
   不是一个可以划过去的回复。所以用遮罩 + 居中面板，必须点选才能继续。 */

.clarify-mask {
  position: fixed;
  inset: 0;
  z-index: 2000;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  background: rgba(8, 8, 18, 0.72);
  backdrop-filter: blur(6px);
  animation: clarify-fade 0.16s ease-out;
}

.clarify-modal {
  width: 100%;
  max-width: 520px;
  max-height: 82vh;
  overflow-y: auto;
  padding: 22px 24px 18px;
  border-radius: 16px;
  background: rgba(26, 28, 48, 0.98);
  border: 1px solid rgba(120, 150, 255, 0.28);
  box-shadow: 0 24px 64px rgba(0, 0, 0, 0.55), 0 0 0 1px rgba(255, 255, 255, 0.03) inset;
  animation: clarify-pop 0.18s cubic-bezier(0.16, 1, 0.3, 1);
}

@keyframes clarify-fade {
  from { opacity: 0; }
  to { opacity: 1; }
}

@keyframes clarify-pop {
  from { opacity: 0; transform: translateY(10px) scale(0.97); }
  to { opacity: 1; transform: translateY(0) scale(1); }
}

.clarify-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 14px;
}

.clarify-tag {
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 9px;
  color: #9db4ff;
  background: rgba(120, 150, 255, 0.14);
  border: 1px solid rgba(120, 150, 255, 0.28);
}

.clarify-count {
  font-size: 11px;
  color: rgba(255, 255, 255, 0.32);
}

.clarify-question {
  font-size: 17px;
  font-weight: 600;
  line-height: 1.5;
  color: rgba(255, 255, 255, 0.94);
  margin-bottom: 6px;
}

.clarify-reason {
  font-size: 12px;
  line-height: 1.6;
  color: rgba(255, 255, 255, 0.45);
  margin-bottom: 16px;
}

.clarify-options {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.clarify-option {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
  padding: 12px 14px;
  border-radius: 11px;
  cursor: pointer;
  text-align: left;
  font-size: 14px;
  font-family: inherit;
  color: rgba(255, 255, 255, 0.88);
  background: rgba(120, 150, 255, 0.08);
  border: 1px solid rgba(120, 150, 255, 0.24);
  transition: background 0.15s, border-color 0.15s, transform 0.1s;
}

.clarify-option:hover:not(:disabled) {
  background: rgba(120, 150, 255, 0.18);
  border-color: rgba(120, 150, 255, 0.6);
  transform: translateX(2px);
}

.clarify-option:active:not(:disabled) {
  transform: translateX(2px) scale(0.995);
}

.clarify-option:focus-visible {
  outline: 2px solid rgba(120, 150, 255, 0.7);
  outline-offset: 2px;
}

.clarify-option:disabled {
  cursor: default;
  opacity: 0.45;
}

.clarify-index {
  flex-shrink: 0;
  width: 22px;
  height: 22px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 7px;
  font-size: 11px;
  font-weight: 600;
  color: #9db4ff;
  background: rgba(120, 150, 255, 0.16);
}

.clarify-option-text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.clarify-option-label {
  font-weight: 500;
}

.clarify-option-hint {
  font-size: 11px;
  color: rgba(255, 255, 255, 0.42);
}

/* ---- 「其他」：自己填一项 ---- */

.clarify-other {
  display: flex;
  gap: 8px;
  margin-top: 10px;
}

.clarify-other-input {
  flex: 1;
  min-width: 0;
  padding: 10px 12px;
  border-radius: 10px;
  font-size: 13px;
  font-family: inherit;
  color: rgba(255, 255, 255, 0.9);
  background: rgba(255, 255, 255, 0.04);
  border: 1px solid rgba(255, 255, 255, 0.12);
  transition: border-color 0.15s, background 0.15s;
}

.clarify-other-input::placeholder {
  color: rgba(255, 255, 255, 0.3);
}

.clarify-other-input:focus {
  outline: none;
  background: rgba(255, 255, 255, 0.06);
  border-color: rgba(120, 150, 255, 0.55);
}

.clarify-other-input:disabled {
  opacity: 0.5;
}

.clarify-other-btn {
  flex-shrink: 0;
  padding: 10px 18px;
  border-radius: 10px;
  font-size: 13px;
  font-family: inherit;
  font-weight: 500;
  cursor: pointer;
  color: #cbd8ff;
  background: rgba(120, 150, 255, 0.16);
  border: 1px solid rgba(120, 150, 255, 0.38);
  transition: background 0.15s, border-color 0.15s;
}

.clarify-other-btn:hover:not(:disabled) {
  background: rgba(120, 150, 255, 0.26);
  border-color: rgba(120, 150, 255, 0.65);
}

.clarify-other-btn:disabled {
  cursor: default;
  opacity: 0.4;
}

.clarify-foot {
  margin-top: 14px;
  min-height: 16px;
}

.clarify-tip,
.clarify-loading {
  font-size: 12px;
  color: rgba(255, 255, 255, 0.4);
}

.clarify-loading {
  color: #9db4ff;
}

/* ---- 本地工作区（连接状态 + 换目录）---- */

.ws-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.ws-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.28);
  flex-shrink: 0;
}

.ws-chip.on .ws-dot {
  background: #4ade80;
  box-shadow: 0 0 6px rgba(74, 222, 128, 0.7);
}

.ws-mask {
  position: fixed;
  inset: 0;
  z-index: 2000;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  background: rgba(8, 8, 18, 0.72);
  backdrop-filter: blur(6px);
}

.ws-modal {
  width: 100%;
  max-width: 560px;
  max-height: 82vh;
  overflow-y: auto;
  padding: 22px 24px 20px;
  border-radius: 16px;
  background: rgba(26, 28, 48, 0.98);
  border: 1px solid rgba(120, 150, 255, 0.28);
  box-shadow: 0 24px 64px rgba(0, 0, 0, 0.55);
  color: rgba(255, 255, 255, 0.86);
  font-size: 13px;
  line-height: 1.7;
}

.ws-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 14px;
}

.ws-tag {
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 9px;
  color: #9db4ff;
  background: rgba(120, 150, 255, 0.14);
  border: 1px solid rgba(120, 150, 255, 0.28);
}

.ws-close {
  border: none;
  background: transparent;
  color: rgba(255, 255, 255, 0.45);
  font-size: 20px;
  line-height: 1;
  cursor: pointer;
}

.ws-close:hover { color: #fff; }

.ws-state { font-size: 14px; font-weight: 600; margin-bottom: 6px; }
.ws-state.ok { color: #4ade80; }
.ws-state.warn { color: #fbbf24; font-weight: 500; font-size: 13px; }

.ws-path {
  font-family: ui-monospace, Consolas, monospace;
  font-size: 12px;
  word-break: break-all;
  padding: 8px 10px;
  border-radius: 8px;
  background: rgba(255, 255, 255, 0.05);
  border: 1px solid rgba(255, 255, 255, 0.08);
  margin-bottom: 10px;
}

.ws-tip { font-size: 12.5px; color: rgba(255, 255, 255, 0.6); margin-bottom: 10px; }
.ws-hint { font-size: 11.5px; color: rgba(255, 255, 255, 0.38); margin-top: 10px; }

.ws-steps { margin: 0 0 4px 18px; font-size: 12.5px; color: rgba(255, 255, 255, 0.7); }
.ws-steps li { margin-bottom: 6px; }

.ws-modal code {
  font-family: ui-monospace, Consolas, monospace;
  font-size: 11.5px;
  padding: 1px 5px;
  border-radius: 4px;
  background: rgba(120, 150, 255, 0.14);
  color: #cbd8ff;
}

.ws-modal pre {
  margin: 6px 0 0;
  padding: 8px 10px;
  border-radius: 8px;
  background: rgba(0, 0, 0, 0.28);
  font-family: ui-monospace, Consolas, monospace;
  font-size: 11.5px;
  color: #cbd8ff;
  overflow-x: auto;
  white-space: pre-wrap;
  word-break: break-all;
}

.ws-actions { display: flex; gap: 8px; margin-top: 12px; }

.ws-btn {
  padding: 8px 16px;
  border-radius: 9px;
  font-size: 12.5px;
  font-family: inherit;
  cursor: pointer;
  color: rgba(255, 255, 255, 0.8);
  background: rgba(255, 255, 255, 0.06);
  border: 1px solid rgba(255, 255, 255, 0.12);
  transition: background 0.15s, border-color 0.15s;
}

.ws-btn:hover:not(:disabled) { background: rgba(255, 255, 255, 0.1); }
.ws-btn:disabled { cursor: default; opacity: 0.5; }

.ws-btn.primary {
  color: #cbd8ff;
  background: rgba(120, 150, 255, 0.16);
  border-color: rgba(120, 150, 255, 0.4);
}

.ws-btn.primary:hover:not(:disabled) { background: rgba(120, 150, 255, 0.26); }

.bubble-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}

.bubble-role {
  font-size: 11px;
  font-weight: 600;
  opacity: 0.6;
  text-transform: uppercase;
  letter-spacing: 0.5px;
}

.streaming-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #a78bfa;
  animation: pulse 0.8s ease-in-out infinite;
}

@keyframes pulse {
  0%, 100% { opacity: 0.3; transform: scale(0.8); }
  50%      { opacity: 1; transform: scale(1.2); }
}

.bubble-content {
  white-space: pre-wrap;
  word-break: break-word;
}

.bubble-content :deep(code) {
  background: rgba(124, 58, 237, 0.2);
  padding: 2px 6px;
  border-radius: 4px;
  font-size: 13px;
}

.bubble-content :deep(strong) {
  color: inherit;
}

/* ==================== Input ==================== */

.input-bar {
  padding: 16px 24px 20px;
  background: rgba(22, 22, 40, 0.7);
  backdrop-filter: blur(20px);
  border-top: 1px solid rgba(255, 255, 255, 0.05);
  display: flex;
  align-items: flex-end;
  gap: 12px;
  z-index: 3;
}

.chat-textarea {
  flex: 1;
  resize: none;
  border: 1px solid rgba(255, 255, 255, 0.08);
  border-radius: 14px;
  padding: 14px 18px;
  font-size: 14px;
  outline: none;
  background: rgba(255, 255, 255, 0.04);
  color: white;
  transition: all 0.25s;
  font-family: inherit;
  min-height: 20px;
  max-height: 160px;
  line-height: 1.5;
}

.chat-textarea::placeholder {
  color: rgba(255, 255, 255, 0.2);
}

.chat-textarea:focus {
  border-color: #7c3aed;
  background: rgba(124, 58, 237, 0.05);
  box-shadow: 0 0 0 3px rgba(124, 58, 237, 0.12);
}

.send-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 46px;
  height: 46px;
  border-radius: 14px;
  border: none;
  background: linear-gradient(135deg, #7c3aed, #a855f7);
  color: white;
  cursor: pointer;
  transition: all 0.3s;
  flex-shrink: 0;
}

.send-btn:hover:not(:disabled) {
  transform: translateY(-2px);
  box-shadow: 0 8px 24px rgba(124, 58, 237, 0.35);
}

.send-btn:disabled {
  background: rgba(255, 255, 255, 0.06);
  color: rgba(255, 255, 255, 0.15);
  cursor: not-allowed;
  transform: none;
  box-shadow: none;
}

/* ==================== Responsive ==================== */

@media (max-width: 768px) {
  .sidebar { width: 200px; }
  .messages { padding: 20px 16px; }
  .topbar { padding: 10px 16px; flex-wrap: wrap; gap: 8px; }
  .input-bar { padding: 12px 16px; }
}
</style>
