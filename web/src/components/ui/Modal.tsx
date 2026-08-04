import type { ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { X } from 'lucide-react'
import { glassPanel, iconBtn } from '../../ui'

/**
 * 居中浮层。
 *
 * **必须挂到 document.body，不能就地渲染。**
 * CSS 规范里 backdrop-filter 非 none 的元素会成为其后代中 position: fixed
 * 元素的包含块——而 .glass（card / glassPanel 都用它）带 backdrop-blur。
 * 就地渲染时，只要弹窗被放进任何一张卡片里，它的 fixed inset-0 就变成相对那张
 * 卡片定位：弹窗跟着页面滚动、被裁在卡片范围内。
 *
 * 这个坑在「执行审计」的详情弹窗上真实出现过，而同一个 Modal 在别处正常——
 * 差别只是渲染位置恰好在不在 glass 容器里。修在调用处只能治一次，
 * 挂 body 才能让所有使用者都不必再关心自己被放在哪。
 */
export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 p-4 backdrop-blur-sm" onClick={onClose}>
      <div
        className={`${glassPanel} max-h-[85vh] w-full max-w-md overflow-y-auto rounded-2xl p-5`}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-4 flex items-center justify-between">
          <h3 className="font-semibold">{title}</h3>
          <button onClick={onClose} className={iconBtn}>
            <X className="h-4 w-4" />
          </button>
        </div>
        {children}
      </div>
    </div>,
    document.body,
  )
}
