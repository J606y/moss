import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import './index.css'

// iOS Safari 只在文档存在 touch 监听时才对元素应用 :active。缺了这一行，
// 全站的按下反馈在 iPhone / iPad 上静默失效，而桌面浏览器一切正常——
// 属于只能在真机上发现的差异，故在此显式兜住，不要当成无用代码删掉。
// passive 表明不会 preventDefault，浏览器可继续走快速滚动路径。
document.addEventListener('touchstart', () => {}, { passive: true })

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
