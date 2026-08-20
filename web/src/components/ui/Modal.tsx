import { useRef, type ReactNode } from 'react'
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
  // 点遮罩关闭，判据是「按下」而不是「点击」。
  //
  // click 的 target 是 mousedown 与 mouseup 两处的最近公共祖先：在输入框里按住
  // 拖选文字、松手时指针已经滑出输入框，这一下就落在遮罩上，弹窗当场关掉，
  // 用户填了一半的表单随之丢失。选文字是编辑弹窗里最平常的动作，不能这么处理。
  // 只有按下和松开都在遮罩上，才算真的点了外面。
  const downOnBackdrop = useRef(false)

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 p-4 backdrop-blur-sm"
      onMouseDown={(e) => {
        downOnBackdrop.current = e.target === e.currentTarget
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget && downOnBackdrop.current) onClose()
      }}
    >
      <div className={`${glassPanel} max-h-[85vh] w-full max-w-md overflow-y-auto rounded-2xl p-5`}>
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
