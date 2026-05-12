<template>
  <div class="page">
    <div class="particles">
      <span v-for="i in 24" :key="i" class="dot" :style="dotStyle(i)"></span>
    </div>

    <header class="topbar">
      <div class="topbar-brand">
        <svg viewBox="0 0 64 64" fill="none" class="topbar-logo">
          <rect x="8" y="20" width="14" height="24" rx="4" fill="white" opacity="0.9"/>
          <rect x="26" y="8" width="14" height="36" rx="4" fill="white" opacity="0.7"/>
          <rect x="44" y="14" width="14" height="30" rx="4" fill="white" opacity="0.5"/>
          <circle cx="16" cy="14" r="4" fill="#a78bfa"/>
          <circle cx="34" cy="4" r="3.5" fill="#c084fc"/>
          <circle cx="52" cy="9" r="3" fill="#e879f9"/>
        </svg>
        <span class="topbar-name">DeepTalk</span>
        <span class="topbar-desc">AI 应用平台</span>
      </div>
      <button class="logout-btn" @click="handleLogout">
        <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4"/>
          <polyline points="16 17 21 12 16 7"/>
          <line x1="21" y1="12" x2="9" y2="12"/>
        </svg>
        退出登录
      </button>
    </header>

    <main class="main">
      <h2 class="section-title">选择功能模块</h2>
      <div class="card-grid">
        <div class="card" @click="$router.push('/ai-chat')">
          <div class="card-icon chat-icon">
            <svg viewBox="0 0 48 48" fill="none">
              <rect x="6" y="8" width="36" height="26" rx="5" stroke="currentColor" stroke-width="2.5"/>
              <circle cx="16" cy="21" r="2" fill="currentColor"/>
              <circle cx="24" cy="21" r="2" fill="currentColor"/>
              <circle cx="32" cy="21" r="2" fill="currentColor"/>
              <path d="M24 34l-6 7v-7h6z" fill="currentColor"/>
            </svg>
          </div>
          <h3>AI 聊天</h3>
          <p>多模型智能对话 · 流式响应 · RAG 增强</p>
          <div class="card-badge">DeepSeek / 通义千问</div>
          <span class="card-arrow">
            <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M5 12h14"/><path d="M12 5l7 7-7 7"/></svg>
          </span>
        </div>

        <div class="card" @click="$router.push('/image-recognition')">
          <div class="card-icon img-icon">
            <svg viewBox="0 0 48 48" fill="none">
              <rect x="4" y="8" width="40" height="32" rx="4" stroke="currentColor" stroke-width="2.5"/>
              <circle cx="16" cy="20" r="5" stroke="currentColor" stroke-width="1.5"/>
              <path d="M4 36l12-10 8 6 6-3 14 7" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
            </svg>
          </div>
          <h3>图像识别</h3>
          <p>上传图片 · AI 智能分析 · 精准识别</p>
          <div class="card-badge">MobileNetV2</div>
          <span class="card-arrow">
            <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M5 12h14"/><path d="M12 5l7 7-7 7"/></svg>
          </span>
        </div>
      </div>
    </main>

    <footer class="footer">
      <span>DeepTalk &copy; 2026</span>
    </footer>
  </div>
</template>

<script>
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'

export default {
  name: 'MenuView',
  setup() {
    const router = useRouter()

    const dotStyle = (i) => {
      const size = 2 + (i % 3) * 1.5
      return {
        width: size + 'px',
        height: size + 'px',
        left: ((i * 37 + 13) % 100) + '%',
        top: ((i * 53 + 7) % 100) + '%',
        animationDelay: (i * 0.7) + 's',
        animationDuration: (4 + (i % 5)) + 's',
        opacity: 0.08 + (i % 3) * 0.05
      }
    }

    const handleLogout = async () => {
      try {
        await ElMessageBox.confirm('确定要退出登录吗？', '提示', {
          confirmButtonText: '确定',
          cancelButtonText: '取消',
          type: 'warning'
        })
        localStorage.removeItem('token')
        ElMessage.success('退出登录成功')
        router.push('/login')
      } catch {
        // 用户取消
      }
    }

    return { dotStyle, handleLogout }
  }
}
</script>

<style scoped>
.page {
  min-height: 100vh;
  background: #0f0f1a;
  display: flex;
  flex-direction: column;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  position: relative;
  overflow: hidden;
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

/* ==================== Top Bar ==================== */

.topbar {
  position: relative;
  z-index: 2;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 36px;
  background: rgba(26, 26, 46, 0.7);
  backdrop-filter: blur(20px);
  border-bottom: 1px solid rgba(255, 255, 255, 0.06);
  animation: barIn 0.5s ease-out;
}

@keyframes barIn {
  from { opacity: 0; transform: translateY(-16px); }
  to   { opacity: 1; transform: translateY(0); }
}

.topbar-brand {
  display: flex;
  align-items: center;
  gap: 12px;
}

.topbar-logo {
  width: 36px;
  height: 36px;
}

.topbar-name {
  color: white;
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.3px;
}

.topbar-desc {
  color: rgba(255, 255, 255, 0.3);
  font-size: 13px;
  padding-left: 12px;
  border-left: 1px solid rgba(255, 255, 255, 0.1);
}

.logout-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 20px;
  border-radius: 12px;
  border: 1px solid rgba(255, 255, 255, 0.1);
  background: rgba(255, 255, 255, 0.04);
  color: rgba(255, 255, 255, 0.55);
  font-size: 14px;
  font-weight: 500;
  cursor: pointer;
  transition: all 0.3s;
}

.logout-btn:hover {
  background: rgba(239, 68, 68, 0.12);
  border-color: rgba(239, 68, 68, 0.3);
  color: #f87171;
  transform: translateY(-1px);
  box-shadow: 0 4px 16px rgba(239, 68, 68, 0.12);
}

/* ==================== Main ==================== */

.main {
  flex: 1;
  position: relative;
  z-index: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 48px 40px 32px;
  animation: mainIn 0.7s cubic-bezier(0.16, 1, 0.3, 1) 0.2s both;
}

@keyframes mainIn {
  from { opacity: 0; transform: translateY(30px); }
  to   { opacity: 1; transform: translateY(0); }
}

.section-title {
  color: rgba(255, 255, 255, 0.7);
  font-size: 15px;
  font-weight: 500;
  letter-spacing: 4px;
  text-transform: uppercase;
  margin: 0 0 40px;
}

.card-grid {
  display: flex;
  gap: 32px;
  max-width: 760px;
  width: 100%;
}

.card {
  flex: 1;
  background: rgba(26, 26, 46, 0.8);
  backdrop-filter: blur(20px);
  border-radius: 20px;
  border: 1px solid rgba(255, 255, 255, 0.06);
  padding: 40px 32px 32px;
  cursor: pointer;
  position: relative;
  overflow: hidden;
  transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1);
  animation: cardUp 0.7s cubic-bezier(0.16, 1, 0.3, 1) both;
}

.card:nth-child(1) { animation-delay: 0.3s; }
.card:nth-child(2) { animation-delay: 0.45s; }

@keyframes cardUp {
  from { opacity: 0; transform: translateY(40px); }
  to   { opacity: 1; transform: translateY(0); }
}

.card::before {
  content: '';
  position: absolute;
  inset: 0;
  border-radius: 20px;
  opacity: 0;
  transition: opacity 0.4s;
}

.card:nth-child(1)::before {
  background: radial-gradient(circle at 100% 0%, rgba(124, 58, 237, 0.12), transparent 70%);
}

.card:nth-child(2)::before {
  background: radial-gradient(circle at 100% 0%, rgba(34, 197, 94, 0.1), transparent 70%);
}

.card:hover::before {
  opacity: 1;
}

.card:hover {
  transform: translateY(-8px);
  border-color: rgba(255, 255, 255, 0.12);
  box-shadow: 0 20px 60px rgba(0, 0, 0, 0.4);
}

.card:nth-child(1):hover { border-color: rgba(124, 58, 237, 0.3); }
.card:nth-child(2):hover { border-color: rgba(34, 197, 94, 0.25); }

.card-icon {
  width: 56px;
  height: 56px;
  border-radius: 16px;
  display: flex;
  align-items: center;
  justify-content: center;
  margin-bottom: 24px;
  transition: all 0.3s;
}

.chat-icon {
  background: rgba(124, 58, 237, 0.12);
  color: #a78bfa;
  box-shadow: 0 0 24px rgba(124, 58, 237, 0.08);
}

.card:hover .chat-icon {
  background: rgba(124, 58, 237, 0.2);
  box-shadow: 0 0 32px rgba(124, 58, 237, 0.15);
}

.img-icon {
  background: rgba(34, 197, 94, 0.1);
  color: #4ade80;
  box-shadow: 0 0 24px rgba(34, 197, 94, 0.06);
}

.card:hover .img-icon {
  background: rgba(34, 197, 94, 0.18);
  box-shadow: 0 0 32px rgba(34, 197, 94, 0.12);
}

.card h3 {
  color: white;
  font-size: 20px;
  font-weight: 700;
  margin: 0 0 10px;
}

.card p {
  color: rgba(255, 255, 255, 0.4);
  font-size: 13px;
  line-height: 1.5;
  margin: 0 0 20px;
}

.card-badge {
  display: inline-block;
  padding: 4px 12px;
  border-radius: 20px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.3px;
}

.card:nth-child(1) .card-badge {
  background: rgba(124, 58, 237, 0.12);
  color: #c4b5fd;
}

.card:nth-child(2) .card-badge {
  background: rgba(34, 197, 94, 0.1);
  color: #86efac;
}

.card-arrow {
  position: absolute;
  right: 24px;
  bottom: 28px;
  color: rgba(255, 255, 255, 0.15);
  transition: all 0.3s;
}

.card:hover .card-arrow {
  color: rgba(255, 255, 255, 0.5);
  transform: translateX(4px);
}

/* ==================== Footer ==================== */

.footer {
  position: relative;
  z-index: 1;
  text-align: center;
  padding: 20px;
  color: rgba(255, 255, 255, 0.15);
  font-size: 12px;
  animation: mainIn 0.7s cubic-bezier(0.16, 1, 0.3, 1) 0.6s both;
}

/* ==================== Responsive ==================== */

@media (max-width: 640px) {
  .card-grid {
    flex-direction: column;
  }
  .topbar {
    padding: 14px 20px;
  }
  .topbar-desc {
    display: none;
  }
  .main {
    padding: 32px 20px;
  }
}
</style>
