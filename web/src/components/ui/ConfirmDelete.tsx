import type { ReactNode } from 'react'
import { Modal } from './Modal'
import { btnDanger, btnGhost } from '../../ui'

/** 删除确认弹窗：外壳 + 取消/删除按钮统一，具体提示文案由 children 传入 */
export function ConfirmDelete({
  title,
  onCancel,
  onConfirm,
  children,
}: {
  title: string
  onCancel: () => void
  onConfirm: () => void
  children: ReactNode
}) {
  return (
    <Modal title={title} onClose={onCancel}>
      <p className="text-sm">{children}</p>
      <div className="mt-4 flex justify-end gap-2">
        <button className={btnGhost} onClick={onCancel}>
          取消
        </button>
        <button className={btnDanger} onClick={onConfirm}>
          删除
        </button>
      </div>
    </Modal>
  )
}
