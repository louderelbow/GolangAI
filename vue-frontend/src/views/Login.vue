<template>
  <div class="page">
    <div class="split">
      <div class="brand">
        <div class="particles">
          <span v-for="i in 20" :key="i" class="dot" :style="dotStyle(i)"></span>
        </div>
        <div class="brand-inner">
          <div class="logo-icon">
            <svg viewBox="0 0 64 64" fill="none">
              <rect x="8" y="20" width="14" height="24" rx="4" fill="white" opacity="0.9"/>
              <rect x="26" y="8" width="14" height="36" rx="4" fill="white" opacity="0.7"/>
              <rect x="44" y="14" width="14" height="30" rx="4" fill="white" opacity="0.5"/>
              <circle cx="16" cy="14" r="4" fill="#a78bfa"/>
              <circle cx="34" cy="4" r="3.5" fill="#c084fc"/>
              <circle cx="52" cy="9" r="3" fill="#e879f9"/>
            </svg>
          </div>
          <h1 class="app-name">DeepTalk</h1>
          <p class="tagline">智能对话 · 多模型接入 · 语音交互</p>
          <div class="features">
            <div class="feat" v-for="(f, i) in features" :key="i" :style="{ animationDelay: `${0.4 + i * 0.15}s` }">
              <span class="feat-dot"></span>
              <span>{{ f }}</span>
            </div>
          </div>
        </div>
      </div>

      <div class="form-side">
        <div class="form-card">
          <div class="form-header">
            <h2>欢迎回来</h2>
            <p>登录您的账号以继续使用</p>
          </div>

          <el-form ref="loginFormRef" :model="loginForm" :rules="loginRules" @keyup.enter="handleLogin">
            <div class="field">
              <label class="field-label">用户名</label>
              <el-input
                v-model="loginForm.username"
                placeholder="请输入您的用户名"
                size="large"
                :prefix-icon="UserIcon"
              />
            </div>
            <div class="field">
              <label class="field-label">密码</label>
              <el-input
                v-model="loginForm.password"
                placeholder="请输入密码"
                type="password"
                show-password
                size="large"
                :prefix-icon="LockIcon"
              />
            </div>

            <el-button
              type="primary"
              size="large"
              :loading="loading"
              @click="handleLogin"
              class="submit-btn"
            >
              {{ loading ? '登录中...' : '登 录' }}
            </el-button>
          </el-form>

          <div class="form-footer">
            还没有账号？
            <router-link to="/register" class="link">立即注册</router-link>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script>
import { ref, reactive, h } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import api from '../utils/api'

const UserIcon = () => h('svg', { viewBox: '0 0 24 24', width: 18, height: 18, fill: 'none', stroke: 'currentColor', 'stroke-width': 1.8, 'stroke-linecap': 'round' }, [
  h('circle', { cx: 12, cy: 8, r: 4 }),
  h('path', { d: 'M6 21v-2a4 4 0 014-4h4a4 4 0 014 4v2' })
])

const LockIcon = () => h('svg', { viewBox: '0 0 24 24', width: 18, height: 18, fill: 'none', stroke: 'currentColor', 'stroke-width': 1.8, 'stroke-linecap': 'round' }, [
  h('rect', { x: 3, y: 11, width: 18, height: 11, rx: 2 }),
  h('path', { d: 'M7 11V7a5 5 0 0110 0v4' })
])

export default {
  name: 'LoginView',
  setup() {
    const router = useRouter()
    const loginFormRef = ref()
    const loading = ref(false)
    const loginForm = reactive({
      username: '',
      password: ''
    })

    const features = [
      '多模型 AI 对话（DeepSeek / 通义千问）',
      '流式实时响应 + SSE 推送',
      'RAG 文档增强问答',
      '文字转语音播报'
    ]

    const loginRules = {
      username: [
        { required: true, message: '请输入用户名', trigger: 'blur' }
      ],
      password: [
        { required: true, message: '请输入密码', trigger: 'blur' },
        { min: 6, message: '密码长度不能少于6位', trigger: 'blur' }
      ]
    }

    const dotStyle = (i) => {
      const size = 2 + (i % 3) * 1.5
      return {
        width: size + 'px',
        height: size + 'px',
        left: ((i * 37 + 13) % 100) + '%',
        top: ((i * 53 + 7) % 100) + '%',
        animationDelay: (i * 0.7) + 's',
        animationDuration: (3 + (i % 4)) + 's',
        opacity: 0.15 + (i % 3) * 0.1
      }
    }

    const handleLogin = async () => {
      try {
        await loginFormRef.value.validate()
        loading.value = true
        const response = await api.post('/user/login', {
          username: loginForm.username,
          password: loginForm.password
        })
        if (response.data.status_code === 1000) {
          localStorage.setItem('token', response.data.token)
          ElMessage.success('登录成功')
          router.push('/menu')
        } else {
          ElMessage.error(response.data.status_msg || '登录失败')
        }
      } catch (error) {
        console.error('Login error:', error)
        ElMessage.error('登录失败，请重试')
      } finally {
        loading.value = false
      }
    }

    return {
      loginFormRef, loading, loginForm, loginRules,
      features, handleLogin, dotStyle,
      UserIcon, LockIcon
    }
  }
}
</script>

<style scoped>
.page {
  min-height: 100vh;
  background: #0f0f1a;
  display: flex;
  align-items: center;
  justify-content: center;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
}

.split {
  display: flex;
  width: 960px;
  min-height: 580px;
  border-radius: 24px;
  overflow: hidden;
  box-shadow: 0 32px 80px rgba(0, 0, 0, 0.5);
}

/* ==================== 左侧品牌区 ==================== */

.brand {
  width: 440px;
  background: linear-gradient(160deg, #1a1035 0%, #2d1b5e 30%, #4c2d8c 60%, #6d28d9 100%);
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
}

.particles {
  position: absolute;
  inset: 0;
  pointer-events: none;
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

.brand-inner {
  position: relative;
  z-index: 1;
  text-align: center;
  padding: 48px 40px;
  animation: brandIn 0.8s cubic-bezier(0.16, 1, 0.3, 1);
}

@keyframes brandIn {
  from { opacity: 0; transform: translateY(24px); }
  to   { opacity: 1; transform: translateY(0); }
}

.logo-icon {
  width: 72px;
  height: 72px;
  margin: 0 auto 24px;
  background: rgba(255, 255, 255, 0.08);
  border-radius: 20px;
  padding: 12px;
  box-sizing: border-box;
  backdrop-filter: blur(12px);
  border: 1px solid rgba(255, 255, 255, 0.15);
}

.app-name {
  color: white;
  font-size: 36px;
  font-weight: 700;
  margin: 0 0 8px;
  letter-spacing: -0.5px;
}

.tagline {
  color: rgba(255, 255, 255, 0.6);
  font-size: 14px;
  margin: 0 0 36px;
  letter-spacing: 1px;
}

.features {
  display: flex;
  flex-direction: column;
  gap: 14px;
  text-align: left;
}

.feat {
  display: flex;
  align-items: center;
  gap: 12px;
  color: rgba(255, 255, 255, 0.75);
  font-size: 13px;
  animation: featIn 0.5s cubic-bezier(0.16, 1, 0.3, 1) both;
}

@keyframes featIn {
  from { opacity: 0; transform: translateX(-12px); }
  to   { opacity: 1; transform: translateX(0); }
}

.feat-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #a78bfa;
  flex-shrink: 0;
  box-shadow: 0 0 8px rgba(167, 139, 250, 0.5);
}

/* ==================== 右侧表单区 ==================== */

.form-side {
  flex: 1;
  background: #1a1a2e;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 48px;
}

.form-card {
  width: 100%;
  max-width: 360px;
  animation: formIn 0.6s cubic-bezier(0.16, 1, 0.3, 1) 0.15s both;
}

@keyframes formIn {
  from { opacity: 0; transform: translateX(20px); }
  to   { opacity: 1; transform: translateX(0); }
}

.form-header {
  margin-bottom: 36px;
}

.form-header h2 {
  color: white;
  font-size: 26px;
  font-weight: 700;
  margin: 0 0 6px;
}

.form-header p {
  color: rgba(255, 255, 255, 0.4);
  font-size: 14px;
  margin: 0;
}

.field {
  margin-bottom: 22px;
  --el-input-bg-color: transparent;
}

.field-label {
  display: block;
  color: rgba(255, 255, 255, 0.6);
  font-size: 13px;
  font-weight: 500;
  margin-bottom: 8px;
  letter-spacing: 0.3px;
}

.field :deep(.el-input__wrapper) {
  background: rgba(255, 255, 255, 0.05);
  border: 1px solid rgba(255, 255, 255, 0.08);
  border-radius: 12px;
  box-shadow: none;
  transition: all 0.25s;
}

.field :deep(.el-input__wrapper:hover) {
  border-color: rgba(255, 255, 255, 0.18);
  background: rgba(255, 255, 255, 0.07);
}

.field :deep(.el-input__wrapper.is-focus) {
  border-color: #7c3aed;
  background: rgba(124, 58, 237, 0.06);
  box-shadow: 0 0 0 3px rgba(124, 58, 237, 0.15);
}

.field :deep(.el-input__inner) {
  color: white;
}

.field :deep(.el-input__inner::placeholder) {
  color: rgba(255, 255, 255, 0.25);
}

.field :deep(.el-input__prefix) {
  color: rgba(255, 255, 255, 0.3);
}

.field :deep(.el-input__suffix) {
  color: rgba(255, 255, 255, 0.25);
}

.submit-btn {
  width: 100%;
  height: 48px;
  margin-top: 8px;
  border-radius: 12px;
  font-size: 15px;
  font-weight: 600;
  letter-spacing: 2px;
  background: linear-gradient(135deg, #7c3aed, #a855f7);
  border: none;
  transition: all 0.3s;
}

.submit-btn:hover {
  background: linear-gradient(135deg, #8b5cf6, #c084fc);
  transform: translateY(-1px);
  box-shadow: 0 8px 32px rgba(124, 58, 237, 0.35);
}

.submit-btn:active {
  transform: translateY(0);
}

.form-footer {
  text-align: center;
  margin-top: 28px;
  color: rgba(255, 255, 255, 0.35);
  font-size: 13px;
}

.link {
  color: #a78bfa;
  text-decoration: none;
  font-weight: 500;
  transition: color 0.2s;
}

.link:hover {
  color: #c084fc;
}

/* ==================== Responsive ==================== */

@media (max-width: 768px) {
  .split {
    width: 100%;
    flex-direction: column;
    min-height: 100vh;
    border-radius: 0;
  }
  .brand {
    width: 100%;
    min-height: 240px;
  }
  .form-side {
    padding: 32px 24px;
  }
}
</style>
