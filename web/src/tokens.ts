/**
 * 全站统一色板：把原先散落在 ProgressBar / Charts / ServerDetail 里的颜色常量集中于此。
 * 改配色只改这一处；各处色值与集中前保持完全一致。
 */

// 基础色相
const green = '#10b981'
const sky = '#0ea5e9'
const amber = '#f59e0b'
const rose = '#f43f5e'
const violet = '#8b5cf6'
const teal = '#14b8a6'
const zinc = '#71717a'

// 负载图主色板（Charts.tsx）
export const palette = { green, sky, amber, rose, violet }

// 进度条阈值色：<60 绿 / <85 橙 / 其余 红
export const barColors = { ok: green, warn: amber, danger: rose } as const

// 延迟曲线循环色板（6 色，ServerDetail 每条任务按序取色）
export const pingPalette = [green, sky, amber, violet, rose, teal]

// 图表网格与坐标轴
export const gridStroke = '#88888830'
export const axisStroke = zinc

// 时间轴刷选条与缩略曲线
export const brushStroke = green
export const brushFill = 'rgba(120,120,128,0.12)'
export const timelineStroke = zinc
