# makewand 模式对比基准测试

## 测试矩阵

6 个被测对象 × 5 个案例 = 30 次运行

| 编号 | 被测对象 | 命令示例 |
|------|----------|----------|
| A | Gemini CLI (直接) | `gemini -p "..." > output/` |
| B | Codex CLI (直接) | `codex -q "..." --full-auto` |
| C | Claude Code (直接) | `claude -p "..."` |
| D | makewand Fast | `makewand new --mode fast` |
| E | makewand Balanced | `makewand new --mode balanced` |
| F | makewand Power | `makewand new --mode power` |

---

## 评分维度 (每项 0-5 分, 满分 25)

| 维度 | 说明 |
|------|------|
| **可运行** | 生成后 `npm install && npm start` (或对应命令) 能否零改动运行 |
| **完整度** | 是否生成了全部必需文件 (package.json, 入口, 组件, 样式, 测试等) |
| **代码质量** | 有无明显 bug、安全问题、硬编码、空函数桩 |
| **UI 体验** | 界面是否美观、响应式、可交互 (脚本类项目改为 CLI 体验) |
| **成本** | API 调用费用 (订阅制记 $0; Fast 模式应为 $0) |

---

## 案例 1: 静态倒计时页 (难度 ★☆☆☆☆)

**目标**: 一个单页 HTML 倒计时, 无构建工具, 纯 HTML+CSS+JS。

**Prompt**:
```
创建一个新年倒计时网页:
- 纯 HTML + CSS + JavaScript, 不需要 npm
- 大号居中数字显示 "距离 2027 年还有 XX 天 XX 时 XX 分 XX 秒"
- 深色背景 + 粒子/烟花动画
- 到 0 时显示 "新年快乐!" 动画
- 手机和电脑都好看
```

**验证方法**:
```bash
# 直接用浏览器打开
open index.html          # 应该直接能用
# 检查文件
ls -la                   # 只需 1-3 个文件
# 手机适配
# 用 Chrome DevTools 切 375px 视口
```

**重点对比**:
- makewand 的优势不大 (任务太简单, 单模型就够)
- 预期: 所有工具都能满分, 主要比成本

---

## 案例 2: Todo 应用 (难度 ★★☆☆☆)

**目标**: React Todo 应用, 有 localStorage 持久化。

**Prompt**:
```
创建一个待办事项应用:
- React + Tailwind CSS
- 添加、完成、删除任务
- 任务分类: 工作 / 生活 / 学习
- 数据保存到 localStorage
- 统计面板显示完成率
- 亮色/暗色切换
- 有 package.json 和测试
```

**验证方法**:
```bash
npm install && npm start
npm test
# 手动: 添加 3 个任务 → 完成 1 个 → 删除 1 个 → 刷新页面
# 验证: 数据是否持久化, 分类筛选是否工作, 暗色模式是否切换
```

**重点对比**:
- 代码能否开箱运行 (直接工具常缺 vite 配置或 tailwind 设置)
- makewand cross-model review 能否抓到遗漏的 edge case
- Fast 模式用 cheap 模型, 区别在于 review 质量

---

## 案例 3: Markdown 博客生成器 (难度 ★★★☆☆)

**目标**: Python CLI 工具, 把 Markdown 文件转成静态博客站点。

**Prompt**:
```
创建一个 Markdown 博客生成器 CLI 工具:
- Python 3, 用 Click 做命令行
- 命令: build (编译), serve (本地预览), new (创建新文章)
- 读取 posts/ 目录的 .md 文件
- 生成 HTML 到 dist/ 目录
- 支持 YAML front matter (title, date, tags)
- 首页文章列表按时间倒序
- 文章页有上一篇/下一篇导航
- 内置 CSS 主题 (不依赖外部 CDN)
- 有 requirements.txt 和 setup.py
- 有单元测试
```

**验证方法**:
```bash
pip install -e .
mkdir -p posts && cat > posts/hello.md << 'EOF'
---
title: Hello World
date: 2026-03-03
tags: [test]
---
# Hello
This is a test post.
EOF

blog build
blog serve           # 访问 localhost:8000
python -m pytest     # 测试通过?
```

**重点对比**:
- 中等复杂度, 涉及文件 I/O、模板渲染、CLI 交互
- 直接工具容易漏掉 edge case (空 posts 目录、无 front matter)
- makewand review 步骤能否发现路径处理 bug
- Balanced vs Fast: Balanced 用 mid-tier 模型, 代码质量应更高

---

## 案例 4: 实时聊天室 (难度 ★★★★☆)

**目标**: WebSocket 聊天室, 前后端分离。

**Prompt**:
```
创建一个实时聊天室:
- 前端: React + Tailwind CSS
- 后端: FastAPI + WebSocket
- 功能:
  - 输入昵称即可进入 (不需要注册)
  - 实时消息推送
  - 显示在线用户列表
  - 用户加入/退出通知
  - 消息时间戳
  - 自动滚动到最新消息
- 前端和后端各有 package.json / requirements.txt
- 有 README 说明如何启动
- 后端有基本测试
```

**验证方法**:
```bash
# 终端 1: 启动后端
cd backend && pip install -r requirements.txt && uvicorn main:app --port 8000

# 终端 2: 启动前端
cd frontend && npm install && npm start

# 测试: 打开两个浏览器标签, 不同昵称, 互相发消息
# 验证: 消息实时到达, 在线列表更新, 退出有通知
```

**重点对比**:
- 前后端协调 (WebSocket URL 对齐、CORS 配置)
- 直接工具常在 WebSocket 握手或 CORS 上出错
- makewand auto-fix 循环的价值: 首次运行报错 → 自动修复
- Power vs Balanced: ensemble+judge 能否选出更好的 WebSocket 实现

---

## 案例 5: 费用追踪 Dashboard (难度 ★★★★★)

**目标**: 全栈应用, FastAPI + SQLite + React, 带图表和 CRUD。

**Prompt**:
```
创建一个个人费用追踪看板:
- 后端: FastAPI + SQLite
  - RESTful API: 增删改查费用记录
  - 字段: 金额, 分类(餐饮/交通/购物/娱乐/其他), 日期, 备注
  - 统计接口: 按月汇总, 按分类汇总, 趋势数据
- 前端: React + Tailwind CSS + Recharts
  - 添加/编辑/删除费用
  - 月度饼图 (按分类)
  - 6个月趋势折线图
  - 费用列表 (支持按日期、分类筛选)
  - 月度预算设置 + 超支警告
- 完整的 package.json, requirements.txt
- 后端有 pytest 测试 (至少覆盖 CRUD)
- 前端有基本组件测试
```

**验证方法**:
```bash
# 后端
cd backend && pip install -r requirements.txt && pytest
uvicorn main:app --port 8000 &

# 前端
cd frontend && npm install && npm test
npm start

# 手动验证:
# 1. 添加 5 笔不同分类的费用
# 2. 编辑一笔 → 金额变化反映在图表上
# 3. 删除一笔 → 列表和图表同步更新
# 4. 切换月份 → 图表正确切换
# 5. 设置预算 → 超支时出现警告
```

**重点对比**:
- 最高复杂度: 数据库设计、API CRUD、状态管理、图表数据格式
- 直接工具 **极容易** 前后端 API 不对齐 (字段名、URL 路径)
- makewand cross-model review 能发现 API contract 不一致
- makewand auto-fix 在 `pytest` / `npm test` 失败时自动修复
- Power mode ensemble: 两个模型并行生成, 第三个评审选最优

---

## 执行脚本

```bash
#!/bin/bash
# benchmarks/run.sh — 自动化基准测试执行器
set -euo pipefail

RESULTS_DIR="benchmarks/results/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

CASES=("countdown" "todo" "blog-gen" "chatroom" "expense-tracker")
MODES=("fast" "balanced" "power")

# --- makewand 模式测试 ---
for case_name in "${CASES[@]}"; do
    for mode in "${MODES[@]}"; do
        dir="$RESULTS_DIR/${case_name}/makewand-${mode}"
        mkdir -p "$dir"
        echo "=== $case_name / makewand --mode $mode ==="

        cd "$dir"
        # makewand new 是交互式, 这里用 --debug 记录 trace
        # 实际需要手动操作选模板/输入描述
        timeout 600 makewand new --mode "$mode" --debug 2>"trace.log" || true
        cd - >/dev/null

        echo "Done: $dir"
    done
done

# --- 直接工具对比 ---
PROMPTS_DIR="benchmarks/prompts"

for case_name in "${CASES[@]}"; do
    prompt=$(cat "$PROMPTS_DIR/${case_name}.txt")

    # Gemini CLI
    dir="$RESULTS_DIR/${case_name}/gemini-direct"
    mkdir -p "$dir"
    echo "=== $case_name / gemini direct ==="
    cd "$dir"
    timeout 300 gemini -p "$prompt" 2>"trace.log" || true
    cd - >/dev/null

    # Codex CLI
    dir="$RESULTS_DIR/${case_name}/codex-direct"
    mkdir -p "$dir"
    echo "=== $case_name / codex direct ==="
    cd "$dir"
    timeout 300 codex -q "$prompt" --full-auto 2>"trace.log" || true
    cd - >/dev/null

    # Claude Code
    dir="$RESULTS_DIR/${case_name}/claude-direct"
    mkdir -p "$dir"
    echo "=== $case_name / claude direct ==="
    cd "$dir"
    timeout 300 claude -p "$prompt" 2>"trace.log" || true
    cd - >/dev/null
done

echo ""
echo "Results saved to: $RESULTS_DIR"
echo "Next: run benchmarks/score.sh to evaluate each output"
```

---

## 评分表模板

```
案例: _______________  难度: ★★★☆☆

| 维度 | Gemini | Codex | Claude | Fast | Balanced | Power |
|------|--------|-------|--------|------|----------|-------|
| 可运行 (0-5)   |   |   |   |   |   |   |
| 完整度 (0-5)   |   |   |   |   |   |   |
| 代码质量 (0-5) |   |   |   |   |   |   |
| UI体验 (0-5)   |   |   |   |   |   |   |
| 成本 ($)       |   |   |   |   |   |   |
| **总分 (0-25)**|   |   |   |   |   |   |

备注:
- 可运行: 5=零改动运行 4=改1处 3=改2-3处 2=大改 1=勉强 0=不能运行
- 完整度: 5=全部文件 4=缺1-2个非关键 3=缺关键文件 2=大量缺失 1=只有框架 0=空
- 代码质量: 5=生产级 4=小问题 3=有bug但能用 2=多处bug 1=逻辑错 0=不可用
- UI体验: 5=精美 4=不错 3=能用 2=粗糙 1=难用 0=不显示
- 成本: 直接工具按API价计, 订阅制记$0
```

---

## 预期结论假设

| 案例 | 预期最优 | 原因 |
|------|----------|------|
| 1. 倒计时 | Claude ≈ Balanced ≈ Power | 太简单, 差异不大 |
| 2. Todo | Balanced > Claude > Fast | Review 步骤能抓到 localStorage 边界 |
| 3. 博客生成器 | Balanced > Power > Claude | 中等复杂度, cross-model review 价值最大 |
| 4. 聊天室 | Power > Balanced > Claude | 前后端协调难, auto-fix 循环关键 |
| 5. 费用追踪 | Power > Balanced >> 直接工具 | 复杂全栈, ensemble+judge 选最优实现 |
| Fast 模式 | 接近 Gemini 直接 | 同模型, 但有 review 步骤加成 |

**核心假设**: makewand 的价值随项目复杂度递增:
- ★: 几乎无优势 (单模型已够)
- ★★★: 明显优势 (review 步骤抓 bug)
- ★★★★★: 巨大优势 (auto-fix + ensemble 弥补单模型的前后端不一致)
