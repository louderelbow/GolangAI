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
          <select id="modelType" v-model="selectedModel" class="model-select">
            <option value="1">DeepSeek</option>
            <option value="2">阿里百炼 RAG</option>
            <option value="3">阿里百炼 MCP</option>
          </select>
        </div>
        <div class="topbar-right">
          <label class="stream-label">
            <input type="checkbox" v-model="isStreaming" />
            流式响应
          </label>
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
          :key="index"
          :class="['bubble', message.role === 'user' ? 'bubble-user' : 'bubble-ai']"
        >
          <div class="bubble-meta">
            <span class="bubble-role">{{ message.role === 'user' ? '你' : 'AI' }}</span>
            <button v-if="message.role === 'assistant'" class="tts-btn" @click="playTTS(message.content)">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5"/><path d="M19.07 4.93a10 10 0 010 14.14M15.54 8.46a5 5 0 010 7.07"/></svg>
            </button>
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
  </div>
</template>

<script>
import { ref, nextTick, computed, onMounted } from 'vue'
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
    const selectedModel = ref('2')
    const isStreaming = ref(false)
    const uploading = ref(false)
    const fileInput = ref(null)

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

    const renderMarkdown = (text) => {
      if (!text && text !== '') return ''
      return String(text)
        .replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>')
        .replace(/\*(.*?)\*/g, '<em>$1</em>')
        .replace(/`(.*?)`/g, '<code>$1</code>')
        .replace(/\n/g, '<br>')
    }

    let currentAudio = null

    const stopAudio = () => {
      if (currentAudio) {
        currentAudio.pause()
        currentAudio.currentTime = 0
        currentAudio = null
      }
    }

    const playTTS = async (text) => {
      stopAudio()
      try {
        const createResponse = await api.post('/AI/chat/tts', { text })
        if (createResponse.data && createResponse.data.status_code === 1000 && createResponse.data.task_id) {
          const taskId = createResponse.data.task_id
          const maxAttempts = 30
          const pollInterval = 1000
          let attempts = 0

          const pollResult = async () => {
            const queryResponse = await api.get('/AI/chat/tts/query', { params: { task_id: taskId } })
            if (queryResponse.data && queryResponse.data.status_code === 1000) {
              const taskStatus = queryResponse.data.task_status
              if (taskStatus === 'Success' && queryResponse.data.task_result) {
                stopAudio()
                const audio = new Audio(queryResponse.data.task_result)
                currentAudio = audio
                audio.play()
                return true
              } else if (taskStatus === 'Running' || taskStatus === 'Created') {
                attempts++
                if (attempts < maxAttempts) {
                  await new Promise(resolve => setTimeout(resolve, pollInterval))
                  return await pollResult()
                } else {
                  ElMessage.error('语音合成超时')
                  return true
                }
              } else {
                ElMessage.error('语音合成失败')
                return true
              }
            }
            attempts++
            if (attempts < maxAttempts) {
              await new Promise(resolve => setTimeout(resolve, pollInterval))
              return await pollResult()
            } else {
              ElMessage.error('语音合成超时')
              return true
            }
          }
          await pollResult()
        } else {
          ElMessage.error('无法创建语音合成任务')
        }
      } catch (error) {
        console.error('TTS error:', error)
        ElMessage.error('请求语音接口失败')
      }
    }

    const loadSessions = async () => {
      try {
        const response = await api.get('/AI/chat/sessions')
        if (response.data && response.data.status_code === 1000 && Array.isArray(response.data.sessions)) {
          const sessionMap = {}
          response.data.sessions.forEach(s => {
            const sid = String(s.sessionId)
            sessionMap[sid] = { id: sid, name: s.name || `会话 ${sid}`, messages: [] }
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
      nextTick(() => {
        if (messageInput.value) messageInput.value.focus()
      })
    }

    const switchSession = async (sessionId) => {
      if (!sessionId) return
      currentSessionId.value = String(sessionId)
      tempSession.value = false

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
      const userMessage = { role: 'user', content: inputMessage.value }
      const currentInput = inputMessage.value
      inputMessage.value = ''

      currentMessages.value.push(userMessage)
      await nextTick()
      scrollToBottom()

      try {
        loading.value = true
        if (isStreaming.value) {
          await handleStreaming(currentInput)
        } else {
          await handleNormal(currentInput)
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

    async function handleStreaming(question) {
      const aiMessage = { role: 'assistant', content: '', meta: { status: 'streaming' } }
      const aiMessageIndex = currentMessages.value.length
      currentMessages.value.push(aiMessage)

      if (!tempSession.value && currentSessionId.value && sessions.value[currentSessionId.value]) {
        if (!sessions.value[currentSessionId.value].messages) sessions.value[currentSessionId.value].messages = []
        sessions.value[currentSessionId.value].messages.push({ role: 'assistant', content: '' })
      }

      const url = tempSession.value ? '/api/AI/chat/send-stream-new-session' : '/api/AI/chat/send-stream'
      const headers = { 'Content-Type': 'application/json', 'Authorization': `Bearer ${localStorage.getItem('token') || ''}` }
      const body = tempSession.value
        ? { question: question, modelType: selectedModel.value }
        : { question: question, modelType: selectedModel.value, sessionId: currentSessionId.value }

      try {
        const response = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body) })
        if (!response.ok) { loading.value = false; throw new Error('Network response was not ok') }

        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          const chunk = decoder.decode(value, { stream: true })
          buffer += chunk
          const lines = buffer.split('\n')
          buffer = lines.pop() || ''

          for (const line of lines) {
            const trimmedLine = line.trim()
            if (!trimmedLine) continue
            if (trimmedLine.startsWith('data:')) {
              const data = trimmedLine.slice(5).trim()
              if (data === '[DONE]') {
                loading.value = false
                currentMessages.value[aiMessageIndex].meta = { status: 'done' }
                currentMessages.value = [...currentMessages.value]
              } else if (data.startsWith('{')) {
                try {
                  const parsed = JSON.parse(data)
                  if (parsed.sessionId) {
                    const newSid = String(parsed.sessionId)
                    if (tempSession.value) {
                      sessions.value[newSid] = { id: newSid, name: '新会话', messages: [...currentMessages.value] }
                      currentSessionId.value = newSid
                      tempSession.value = false
                    }
                  }
                } catch (e) {
                  currentMessages.value[aiMessageIndex].content += data
                }
              } else {
                currentMessages.value[aiMessageIndex].content += data
              }
              currentMessages.value = [...currentMessages.value]
              await new Promise(resolve => requestAnimationFrame(() => { scrollToBottom(); resolve() }))
            }
          }
        }

        loading.value = false
        currentMessages.value[aiMessageIndex].meta = { status: 'done' }
        currentMessages.value = [...currentMessages.value]

        if (!tempSession.value && currentSessionId.value && sessions.value[currentSessionId.value]) {
          const sessMsgs = sessions.value[currentSessionId.value].messages
          if (Array.isArray(sessMsgs) && sessMsgs.length) {
            const lastIndex = sessMsgs.length - 1
            if (sessMsgs[lastIndex] && sessMsgs[lastIndex].role === 'assistant') {
              sessMsgs[lastIndex].content = currentMessages.value[aiMessageIndex].content
            }
          }
        }
      } catch (err) {
        console.error('Stream error:', err)
        loading.value = false
        currentMessages.value[aiMessageIndex].meta = { status: 'error' }
        currentMessages.value = [...currentMessages.value]
        ElMessage.error('流式传输出错')
      }
    }

    async function handleNormal(question) {
      if (tempSession.value) {
        const response = await api.post('/AI/chat/send-new-session', { question, modelType: selectedModel.value })
        if (response.data && response.data.status_code === 1000) {
          const sessionId = String(response.data.sessionId)
          const aiMessage = { role: 'assistant', content: response.data.Information || '' }
          sessions.value[sessionId] = { id: sessionId, name: '新会话', messages: [{ role: 'user', content: question }, aiMessage] }
          currentSessionId.value = sessionId
          tempSession.value = false
          currentMessages.value = [...sessions.value[sessionId].messages]
        } else {
          ElMessage.error(response.data?.status_msg || '发送失败')
          currentMessages.value.pop()
        }
      } else {
        const sessionMsgs = sessions.value[currentSessionId.value].messages
        sessionMsgs.push({ role: 'user', content: question })
        const response = await api.post('/AI/chat/send', { question, modelType: selectedModel.value, sessionId: currentSessionId.value })
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

    onMounted(() => { loadSessions() })

    return {
      sessions: computed(() => Object.values(sessions.value)),
      currentSessionId, tempSession, currentMessages, inputMessage, loading,
      messagesRef, messageInput, selectedModel, isStreaming, uploading, fileInput,
      dotStyle, renderMarkdown, playTTS, createNewSession, switchSession, syncHistory,
      sendMessage, triggerFileUpload, handleFileUpload
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

.tts-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  border: none;
  background: rgba(124, 58, 237, 0.15);
  color: #a78bfa;
  cursor: pointer;
  transition: all 0.2s;
}

.tts-btn:hover {
  background: rgba(124, 58, 237, 0.3);
  color: #c4b5fd;
  transform: scale(1.1);
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
