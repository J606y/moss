// 逐位数字：把格式化后的字符串按字符拆开，只有发生变化的「数字位」会重挂并播放一次
// 竖向滚入动画；未变的位 DOM 节点复用、纹丝不动 —— 不再「整段数字一次性重新渲染」。
// 纯 CSS、不引依赖，保留 tabular-nums 等宽与实时刷新。
//
// 关键：数字位的 key 拼上字符本身（`${i}:${c}`），该位一变 React 即重挂这个 span、触发入场动画；
// 未变的位 key 不变、节点复用、不重播。各位互相独立，不强制先后错峰。
export default function Ticker({ value, className = '' }: { value: string; className?: string }) {
  const chars = [...value]
  return (
    <span className={`tabular-nums ${className}`}>
      {chars.map((c, i) =>
        c >= '0' && c <= '9' ? (
          <span key={`${i}:${c}`} className="ticker-digit">
            {c}
          </span>
        ) : (
          <span key={`x${i}`} className="ticker-static">
            {c}
          </span>
        ),
      )}
    </span>
  )
}
