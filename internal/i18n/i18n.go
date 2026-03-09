package i18n

import "sync"

var (
	mu          sync.RWMutex
	currentLang = "en"
)

// Messages holds all translatable strings.
type Messages struct {
	// General
	AppName     string
	AppTagline  string
	Version     string
	Yes         string
	No          string
	Confirm     string
	Cancel      string
	Back        string
	Next        string
	Done        string
	Error       string
	Warning     string
	Quit        string
	QuitConfirm string

	// Wizard
	WizardWelcome      string
	WizardPrompt       string
	WizardPlanning     string
	WizardConfirm      string
	WizardBuilding     string
	WizardDone         string
	WizardTemplate     string
	WizardCustom       string
	WizardDescribeHint string
	WizardConfirmHint  string
	WizardNavHint      string
	WizardErrWorkdir   string
	WizardErrProject   string
	WizardWarnGitInit  string
	WizardProjectMade  string

	// Templates
	TplBlog      string
	TplEcommerce string
	TplDashboard string
	TplChatbot   string
	TplMiniApp   string
	TplScript    string

	// Chat
	ChatWelcome      string
	ChatPrompt       string
	ChatCommandHint  string
	ChatThinking     string
	ChatWorking      string
	ChatPlaceholder  string
	ChatThinkingAnim string

	// Cost
	CostSession      string
	CostMonth        string
	CostFree         string
	CostLocal        string
	CostTotal        string
	CostSubscription string
	CostRequests     string
	CostTokensEst    string

	// Progress
	ProgressTitle          string
	ProgressAnalyzing      string
	ProgressCreating       string
	ProgressTesting        string
	ProgressFixing         string
	ProgressDone           string
	ProgressFilesFound     string
	ProgressFilesWritten   string
	ProgressInstallingDeps string
	ProgressDepsInstalled  string
	ProgressNoDeps         string
	ProgressRunningTests   string
	ProgressTestsPassed    string
	ProgressTestsFailed    string
	ProgressAutoFix        string
	ProgressBuildComplete  string
	ProgressNoTests        string
	ProgressReviewing      string
	ProgressReviewDone     string
	ProgressReviewLGTM     string
	ProgressReviewFixing   string
	ProgressReviewApplied  string
	ProgressCrossModel     string

	// Errors
	ErrNoModel     string
	ErrNoAPIKey    string
	ErrBuildFail   string
	ErrTestFail    string
	ErrAutofix     string
	ErrFileWrite   string
	ErrFileRefresh string
	ErrMaxRetries  string
	ErrCancelled   string

	// File tree
	FileTreeTitle string
	FileTreeEmpty string
	FileTreeMore  string

	// File operations
	FileConfirmWrite string
	FileCancelled    string
	FileWriteCount   string

	// Other
	FallbackNotice string

	// Mode
	ModeFree     string
	ModeEconomy  string
	ModeBalanced string
	ModePower    string
	ModeLabel    string
	ModeChanged  string
	ModeHelp     string

	// Setup
	SetupWelcome  string
	SetupAPIKey   string
	SetupModel    string
	SetupLanguage string
	SetupDone     string
}

var en = Messages{
	AppName:     "makewand",
	AppTagline:  "AI coding assistant for everyone",
	Version:     "v0.1.10",
	Yes:         "Yes",
	No:          "No",
	Confirm:     "Confirm",
	Cancel:      "Cancel",
	Back:        "Back",
	Next:        "Next",
	Done:        "Done",
	Error:       "Error",
	Warning:     "Warning",
	Quit:        "Quit",
	QuitConfirm: "Are you sure you want to quit?",

	WizardWelcome:      "What would you like to build?",
	WizardPrompt:       "Describe your project in plain language:",
	WizardPlanning:     "Let me plan this for you...",
	WizardConfirm:      "Ready to start building?",
	WizardBuilding:     "Building your project...",
	WizardDone:         "Your project is ready!",
	WizardTemplate:     "Pick a template to start:",
	WizardCustom:       "Describe in your own words...",
	WizardDescribeHint: "Type your project description below:",
	WizardConfirmHint:  "Enter/Y confirm • N cancel",
	WizardNavHint:      "↑↓ navigate • Enter select • q quit",
	WizardErrWorkdir:   "Error getting working directory: %s",
	WizardErrProject:   "Error creating project: %s",
	WizardWarnGitInit:  "Warning: git init failed: %s",
	WizardProjectMade:  "Created project: %s",

	TplBlog:      "Personal Blog / Portfolio",
	TplEcommerce: "E-commerce Store",
	TplDashboard: "Data Dashboard",
	TplChatbot:   "Chatbot",
	TplMiniApp:   "Mobile App",
	TplScript:    "Automation Script",

	ChatWelcome:      "Welcome! I'm here to help you build and modify your project.",
	ChatPrompt:       "What would you like to do?",
	ChatCommandHint:  "Commands: /mode [free|economy|balanced|power] | /help | /exit (or Ctrl+C)",
	ChatThinking:     "Thinking...",
	ChatWorking:      "Working on it...",
	ChatPlaceholder:  "Type your message... (Enter to send, Ctrl+D for multiline)",
	ChatThinkingAnim: "Thinking...",

	CostSession:      "Session Cost",
	CostMonth:        "Monthly Total",
	CostFree:         "free",
	CostLocal:        "local",
	CostTotal:        "Total",
	CostSubscription: "subscription",
	CostRequests:     "%d requests (~%s tokens)",
	CostTokensEst:    "~%dK tokens",

	ProgressTitle:          "Progress",
	ProgressAnalyzing:      "Analyzing requirements...",
	ProgressCreating:       "Creating files...",
	ProgressTesting:        "Running tests...",
	ProgressFixing:         "Fixing issues...",
	ProgressDone:           "All done!",
	ProgressFilesFound:     "Found %d files",
	ProgressFilesWritten:   "Wrote %d files",
	ProgressInstallingDeps: "Installing dependencies...",
	ProgressDepsInstalled:  "Dependencies installed",
	ProgressNoDeps:         "No dependencies to install",
	ProgressRunningTests:   "Running tests...",
	ProgressTestsPassed:    "Tests passed",
	ProgressTestsFailed:    "Tests failed",
	ProgressAutoFix:        "Auto-fix attempt %d/%d",
	ProgressBuildComplete:  "Build complete!",
	ProgressNoTests:        "No test framework detected",
	ProgressReviewing:      "Reviewing code...",
	ProgressReviewDone:     "Review complete (%s)",
	ProgressReviewLGTM:     "Code looks good!",
	ProgressReviewFixing:   "Applying review fixes...",
	ProgressReviewApplied:  "Applied %d fixes from %s review",
	ProgressCrossModel:     "%s wrote code, %s reviewing",

	ErrNoModel:     "No AI model configured. Run 'makewand setup' first.",
	ErrNoAPIKey:    "API key not set for %s. Set it with 'makewand setup' or the environment variable.",
	ErrBuildFail:   "Build failed",
	ErrTestFail:    "Tests failed",
	ErrAutofix:     "Don't worry! Let me analyze and fix this...",
	ErrFileWrite:   "Failed to write %s: %s",
	ErrFileRefresh: "Failed to refresh project files: %s",
	ErrMaxRetries:  "Auto-fix failed after %d attempts. Please fix manually.",
	ErrCancelled:   "Operation cancelled",

	FileTreeTitle: "Project Files",
	FileTreeEmpty: "(no files yet)",
	FileTreeMore:  "+%d more files",

	FileConfirmWrite: "Write %d files to disk? (Y/n)",
	FileCancelled:    "File write cancelled",
	FileWriteCount:   "Wrote %d files",

	FallbackNotice: "%s unavailable, using %s instead",

	ModeFree:     "Free",
	ModeEconomy:  "Economy",
	ModeBalanced: "Balanced",
	ModePower:    "Power",
	ModeLabel:    "Mode",
	ModeChanged:  "Mode: %s",
	ModeHelp:     "/mode [free|economy|balanced|power]",

	SetupWelcome:  "Welcome to makewand! Let's set up your AI models.",
	SetupAPIKey:   "Enter your %s API key (or press Enter to skip):",
	SetupModel:    "Choose your default model:",
	SetupLanguage: "Choose your language:",
	SetupDone:     "Setup complete! Run 'makewand new' to create your first project.",
}

var zh = Messages{
	AppName:     "makewand",
	AppTagline:  "人人都能用的 AI 编程助手",
	Version:     "v0.1.10",
	Yes:         "是",
	No:          "否",
	Confirm:     "确认",
	Cancel:      "取消",
	Back:        "返回",
	Next:        "下一步",
	Done:        "完成",
	Error:       "错误",
	Warning:     "警告",
	Quit:        "退出",
	QuitConfirm: "确定要退出吗？",

	WizardWelcome:      "你想构建什么？",
	WizardPrompt:       "用自然语言描述你的项目：",
	WizardPlanning:     "我来帮你规划...",
	WizardConfirm:      "确认开始构建？",
	WizardBuilding:     "正在构建你的项目...",
	WizardDone:         "项目已就绪！",
	WizardTemplate:     "选择一个模板开始：",
	WizardCustom:       "用自己的话描述...",
	WizardDescribeHint: "在下方输入你的项目描述：",
	WizardConfirmHint:  "Enter/Y 确认 • N 取消",
	WizardNavHint:      "↑↓ 导航 • Enter 选择 • q 退出",
	WizardErrWorkdir:   "获取工作目录失败：%s",
	WizardErrProject:   "创建项目失败：%s",
	WizardWarnGitInit:  "警告：git 初始化失败：%s",
	WizardProjectMade:  "已创建项目：%s",

	TplBlog:      "个人博客 / 作品集",
	TplEcommerce: "电商网站",
	TplDashboard: "数据看板",
	TplChatbot:   "聊天机器人",
	TplMiniApp:   "小程序 / 移动端",
	TplScript:    "自动化脚本",

	ChatWelcome:      "欢迎！我来帮你构建和修改项目。",
	ChatPrompt:       "你想做什么？",
	ChatCommandHint:  "命令：/mode [free|economy|balanced|power] | /help | /exit（或 Ctrl+C）",
	ChatThinking:     "正在思考...",
	ChatWorking:      "正在处理...",
	ChatPlaceholder:  "输入消息... (Enter 发送, Ctrl+D 多行)",
	ChatThinkingAnim: "正在思考...",

	CostSession:      "本次费用",
	CostMonth:        "本月累计",
	CostFree:         "免费",
	CostLocal:        "本地",
	CostTotal:        "总计",
	CostSubscription: "订阅",
	CostRequests:     "%d 次请求 (~%s tokens)",
	CostTokensEst:    "~%dK tokens",

	ProgressTitle:          "进度",
	ProgressAnalyzing:      "正在分析需求...",
	ProgressCreating:       "正在创建文件...",
	ProgressTesting:        "正在运行测试...",
	ProgressFixing:         "正在修复问题...",
	ProgressDone:           "全部完成！",
	ProgressFilesFound:     "找到 %d 个文件",
	ProgressFilesWritten:   "已写入 %d 个文件",
	ProgressInstallingDeps: "正在安装依赖...",
	ProgressDepsInstalled:  "依赖安装完成",
	ProgressNoDeps:         "无需安装依赖",
	ProgressRunningTests:   "正在运行测试...",
	ProgressTestsPassed:    "测试通过",
	ProgressTestsFailed:    "测试失败",
	ProgressAutoFix:        "自动修复 第 %d/%d 次",
	ProgressBuildComplete:  "构建完成！",
	ProgressNoTests:        "未检测到测试框架",
	ProgressReviewing:      "正在审查代码...",
	ProgressReviewDone:     "审查完成 (%s)",
	ProgressReviewLGTM:     "代码没有问题！",
	ProgressReviewFixing:   "正在应用审查修复...",
	ProgressReviewApplied:  "应用了 %d 个来自 %s 审查的修复",
	ProgressCrossModel:     "%s 生成代码，%s 审查",

	ErrNoModel:     "未配置 AI 模型。请先运行 'makewand setup'。",
	ErrNoAPIKey:    "未设置 %s 的 API 密钥。用 'makewand setup' 或环境变量设置。",
	ErrBuildFail:   "构建失败",
	ErrTestFail:    "测试失败",
	ErrAutofix:     "不要慌！让我来分析和修复...",
	ErrFileWrite:   "写入 %s 失败: %s",
	ErrFileRefresh: "刷新项目文件失败: %s",
	ErrMaxRetries:  "自动修复在 %d 次尝试后失败，请手动修复。",
	ErrCancelled:   "操作已取消",

	FileTreeTitle: "项目文件",
	FileTreeEmpty: "（暂无文件）",
	FileTreeMore:  "还有 %d 个文件",

	FileConfirmWrite: "写入 %d 个文件到磁盘？(Y/n)",
	FileCancelled:    "已取消文件写入",
	FileWriteCount:   "已写入 %d 个文件",

	FallbackNotice: "%s 不可用，已切换到 %s",

	ModeFree:     "免费",
	ModeEconomy:  "经济",
	ModeBalanced: "平衡",
	ModePower:    "强劲",
	ModeLabel:    "模式",
	ModeChanged:  "模式：%s",
	ModeHelp:     "/mode [free|economy|balanced|power]",

	SetupWelcome:  "欢迎使用 makewand！让我们配置 AI 模型。",
	SetupAPIKey:   "输入你的 %s API 密钥（按回车跳过）：",
	SetupModel:    "选择默认模型：",
	SetupLanguage: "选择语言：",
	SetupDone:     "配置完成！运行 'makewand new' 来创建你的第一个项目。",
}

// SetLanguage sets the current language.
func SetLanguage(lang string) {
	mu.Lock()
	defer mu.Unlock()
	if lang == "zh" || lang == "en" {
		currentLang = lang
	}
}

// GetLanguage returns the current language.
func GetLanguage() string {
	mu.RLock()
	defer mu.RUnlock()
	return currentLang
}

// Msg returns the messages for the current language.
func Msg() *Messages {
	mu.RLock()
	defer mu.RUnlock()
	if currentLang == "zh" {
		return &zh
	}
	return &en
}
