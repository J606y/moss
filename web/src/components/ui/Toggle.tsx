import { CheckBox } from './CheckBox'

export function Toggle({ checked, label, onChange }: { checked: boolean; label: string; onChange: (v: boolean) => void }) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={checked}
      onClick={() => onChange(!checked)}
      className="flex cursor-pointer select-none items-center gap-2 text-left text-sm"
    >
      <CheckBox checked={checked} />
      {label}
    </button>
  )
}
