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

      <div class="session-label">功能模块</div>

      <ul class="session-list">
        <li class="session-item active">
          <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" class="session-icon">
            <rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><polyline points="21 15 16 10 5 21"/>
          </svg>
          <span>图像识别助手</span>
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
        <span class="topbar-title">AI 图像识别</span>
        <span class="topbar-hint">
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/></svg>
          上传图片，AI 将自动识别图像内容
        </span>
      </div>

      <div class="messages" ref="chatContainerRef">
        <div v-if="messages.length === 0" class="empty-state">
          <div class="empty-icon">
            <svg viewBox="0 0 64 64" fill="none"><rect x="8" y="12" width="48" height="40" rx="4" stroke="currentColor" stroke-width="1.5"/><circle cx="22" cy="28" r="5" stroke="currentColor" stroke-width="1.2"/><path d="M8 44l16-12 10 7 8-4 14 9" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>
          </div>
          <p>选择一张图片，开始识别</p>
        </div>

        <div
          v-for="(message, index) in messages"
          :key="index"
          :class="['bubble', message.role === 'user' ? 'bubble-user' : 'bubble-ai']"
        >
          <div class="bubble-meta">
            <span class="bubble-role">{{ message.role === 'user' ? '你' : 'AI' }}</span>
          </div>
          <div class="bubble-content">
            <span>{{ message.content }}</span>
            <img v-if="message.imageUrl" :src="message.imageUrl" alt="上传的图片" class="uploaded-img" />
          </div>
        </div>
      </div>

      <div class="input-bar">
        <form @submit.prevent="handleSubmit" class="upload-form">
          <label class="file-label" :class="{ 'has-file': selectedFile }">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/></svg>
            <span>{{ selectedFile ? selectedFile.name : '点击选择图片' }}</span>
            <input
              ref="fileInputRef"
              type="file"
              accept="image/*"
              required
              @change="handleFileSelect"
              class="file-input-hidden"
            />
          </label>
          <button type="submit" :disabled="!selectedFile" class="submit-btn">
            <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
          </button>
        </form>
      </div>
    </main>
  </div>
</template>

<script>
import { ref, nextTick } from 'vue'
import api from '../utils/api'

export default {
  name: 'ImageRecognition',
  setup() {
    const messages = ref([])
    const selectedFile = ref(null)
    const fileInputRef = ref()
    const chatContainerRef = ref()

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

    const handleFileSelect = (event) => {
      selectedFile.value = event.target.files[0]
    }

    const handleSubmit = async () => {
      if (!selectedFile.value) return

      const file = selectedFile.value
      const imageUrl = URL.createObjectURL(file)

      messages.value.push({
        role: 'user',
        content: `已上传图片: ${file.name}`,
        imageUrl: imageUrl
      })

      await nextTick()
      scrollToBottom()

      const formData = new FormData()
      formData.append('image', file)

      try {
        const response = await api.post('/image/recognize', formData, {
          headers: { 'Content-Type': 'multipart/form-data' }
        })

        if (response.data && response.data.class_name) {
          const aiText = `识别结果: ${response.data.class_name}`
          messages.value.push({ role: 'assistant', content: aiText })
        } else {
          messages.value.push({ role: 'assistant', content: `[错误] ${response.data.status_msg || '识别失败'}` })
        }
      } catch (error) {
        console.error('Upload error:', error)
        messages.value.push({ role: 'assistant', content: `[错误] 无法连接到服务器或上传失败: ${error.message}` })
      } finally {
        URL.revokeObjectURL(imageUrl)
        await nextTick()
        scrollToBottom()
        selectedFile.value = null
        if (fileInputRef.value) fileInputRef.value.value = ''
      }
    }

    const scrollToBottom = () => {
      if (chatContainerRef.value) {
        try { chatContainerRef.value.scrollTop = chatContainerRef.value.scrollHeight } catch (e) { /* scroll not available */ }
      }
    }

    return {
      messages, selectedFile, fileInputRef, chatContainerRef,
      dotStyle, handleFileSelect, handleSubmit
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

.session-label {
  padding: 12px 20px 10px;
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

.session-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 11px 14px;
  margin-bottom: 2px;
  border-radius: 10px;
  color: rgba(255, 255, 255, 0.5);
  font-size: 13px;
}

.session-item.active {
  background: rgba(124, 58, 237, 0.15);
  color: #c4b5fd;
  font-weight: 500;
}

.session-icon {
  flex-shrink: 0;
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

.topbar-title {
  color: white;
  font-size: 15px;
  font-weight: 600;
}

.topbar-hint {
  display: flex;
  align-items: center;
  gap: 6px;
  color: rgba(255, 255, 255, 0.3);
  font-size: 12px;
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

.empty-state {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  color: rgba(255, 255, 255, 0.15);
  gap: 16px;
}

.empty-icon {
  width: 80px;
  height: 80px;
  opacity: 0.4;
}

.empty-state p {
  font-size: 14px;
  margin: 0;
}

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

.bubble-content {
  white-space: pre-wrap;
  word-break: break-word;
}

.uploaded-img {
  max-width: 260px;
  border-radius: 12px;
  display: block;
  margin-top: 12px;
  border: 1px solid rgba(255, 255, 255, 0.08);
}

/* ==================== Input ==================== */

.input-bar {
  padding: 16px 24px 20px;
  background: rgba(22, 22, 40, 0.7);
  backdrop-filter: blur(20px);
  border-top: 1px solid rgba(255, 255, 255, 0.05);
  z-index: 3;
}

.upload-form {
  display: flex;
  align-items: center;
  gap: 12px;
}

.file-label {
  flex: 1;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 18px;
  border: 1px dashed rgba(255, 255, 255, 0.12);
  border-radius: 14px;
  color: rgba(255, 255, 255, 0.3);
  font-size: 13px;
  cursor: pointer;
  transition: all 0.25s;
}

.file-label:hover {
  border-color: rgba(255, 255, 255, 0.25);
  color: rgba(255, 255, 255, 0.5);
  background: rgba(255, 255, 255, 0.02);
}

.file-label.has-file {
  border-style: solid;
  border-color: rgba(34, 197, 94, 0.3);
  color: #4ade80;
  background: rgba(34, 197, 94, 0.05);
}

.file-input-hidden {
  display: none;
}

.submit-btn {
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

.submit-btn:hover:not(:disabled) {
  transform: translateY(-2px);
  box-shadow: 0 8px 24px rgba(124, 58, 237, 0.35);
}

.submit-btn:disabled {
  background: rgba(255, 255, 255, 0.06);
  color: rgba(255, 255, 255, 0.15);
  cursor: not-allowed;
}

/* ==================== Responsive ==================== */

@media (max-width: 768px) {
  .sidebar { width: 200px; }
  .messages { padding: 20px 16px; }
  .topbar { padding: 10px 16px; flex-wrap: wrap; gap: 8px; }
  .topbar-hint { display: none; }
  .input-bar { padding: 12px 16px; }
}
</style>
