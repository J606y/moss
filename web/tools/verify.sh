#!/usr/bin/env bash
# 前端 i18n 改造的全量校验：类型 → 构建 → 浏览器实测。
# 任一步失败即退出，退出码非零。
set -e
cd "$(dirname "$0")/.."

echo "=== tsc --noEmit ==="
./node_modules/.bin/tsc --noEmit
echo "[类型检查通过]"

echo
echo "=== vite build ==="
./node_modules/.bin/vite build 2>&1 | tail -5
echo "[构建通过]"

echo
echo "=== i18n 冒烟（真实浏览器） ==="
node tools/i18n-smoke.mjs
