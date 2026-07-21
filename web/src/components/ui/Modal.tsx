import type { ReactNode } from 'react'
import { X } from 'lucide-react'
import { glassPanel, iconBtn } from '../../ui'

export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  return (
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
    </div>
  )
}
