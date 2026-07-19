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
	ChatWelcome                   string
	ChatPrompt                    string
	ChatCommandHint               string
	ChatThinking                  string
	ChatWorking                   string
	ChatPlaceholder               string
	ChatThinkingAnim              string
	ActivityTitle                 string
	ActivityLabel                 string
	ChatActivityPreparing         string
	ChatActivityContext           string
	ChatActivitySelectingProvider string
	ChatActivitySelectingGeneric  string
	ChatActivityWaitingProvider   string
	ChatActivityWaitingGeneric    string
	ChatActivityStreamingProvider string
	ChatActivityStreamingGeneric  string
	ChatActivityWorking           string
	ChatActivityMultiModel        string
	ChatActivityElapsed           string
	ChatActivityFallback          string
	ChatActivityChunkOne          string
	ChatActivityChunkMany         string
	ChatActivityChars             string
	ApprovalTitle                 string
	ApprovalPendingLabel          string
	ApprovalActionHint            string
	ApprovalNone                  string
	ApprovalPendingWrite          string
	ApprovalPlannedCommand        string
	ApprovalDepsConfirm           string
	ApprovalTestsConfirm          string
	ApprovalModeLabel             string
	ApprovalModeManual            string
	ApprovalModeSafe              string
	ApprovalModeAutopilot         string
	ApprovalModeChanged           string
	ApprovalModeHelp              string
	ApprovalAutoWrite             string
	ApprovalAutoWriteAutopilot    string
	ApprovalAutoDeps              string
	ApprovalAutoTests             string
	ApprovalIsolationUnavailable  string
	AutomationCandidateStarted    string
	AutomationCandidateRunning    string
	AutomationCandidateVerifying  string
	AutomationCandidatePassed     string
	AutomationCandidateRejected   string
	AutomationCandidateFailed     string
	AutomationCandidateCanceled   string
	AutomationCandidateSelected   string
	AutomationCandidateFallback   string

	AutomationCandidateIsolationUnavailable string
	AutomationCandidateWeakVerification     string
	AutomationCandidateDeletions            string
	HostCLIExecNotice                       string
	BuildDepsDetectFailed                   string
	BuildTestsDetectFailed                  string
	BuildDepsExecError                      string
	BuildDepsExecFailed                     string
	BuildDepsSkipped                        string
	BuildTestsSkipped                       string
	ExecDepsLabel                           string
	ExecTestsLabel                          string
	ExecStarted                             string
	ExecFinished                            string
	ExecCommand                             string
	ExecExitCode                            string
	ExecDuration                            string
	ExecOutput                              string
	RestoredSessionPrefix                   string
	RestoredSessionNotice                   string
	NoCompactedMemoryNotice                 string
	MemoryRestoredAt                        string

	// Chat commands and status output
	ChatHelp             string
	CompactDone          string
	CompactNothing       string
	ResumeError          string
	ResumeNotFound       string
	IdentityAnswer       string
	ModelProfileLabel    string
	ModeNotSet           string
	StatusProvidersNone  string
	StatusProviders      string
	StatusProject        string
	StatusProjectEntries string
	StatusSessionFile    string
	StatusLastSaved      string
	StatusSessionCost    string
	Goodbye              string

	// Cost
	CostSession       string
	CostMonth         string
	CostFree          string
	CostLocal         string
	CostTotal         string
	CostSubscription  string
	CostRequests      string
	CostTokensEst     string
	CostSummaryTotal  string
	CostNoRequests    string
	CostProviderLine  string
	BudgetWarningMsg  string
	BudgetExceededMsg string

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

	RepoTrustNoSafeProvider string

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
	ModeFast     string
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

//nolint:gosec // G101 false positive: UI translation strings mention "API key" but contain no credentials.
var en = Messages{
	AppName:     "makewand",
	AppTagline:  "Multi-provider coding router for terminal makers",
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

	ChatWelcome:                   "Welcome! I'm here to help you build and modify your project.",
	ChatPrompt:                    "What would you like to do?",
	ChatCommandHint:               "Commands: /model [fast|balanced|power] | /approval [manual|safe|autopilot] | /clear | /status | /cost | /exit (or Ctrl+D)",
	ChatThinking:                  "Thinking...",
	ChatWorking:                   "Working on it...",
	ChatPlaceholder:               "Type your message... (Enter to send, / for commands)",
	ChatThinkingAnim:              "Thinking...",
	ActivityTitle:                 "Activity",
	ActivityLabel:                 "Activity",
	ChatActivityPreparing:         "Preparing request context",
	ChatActivityContext:           "Collecting project context",
	ChatActivitySelectingProvider: "Selecting %s",
	ChatActivitySelectingGeneric:  "Selecting provider",
	ChatActivityWaitingProvider:   "Waiting for %s to start responding",
	ChatActivityWaitingGeneric:    "Waiting for model response",
	ChatActivityStreamingProvider: "Receiving response from %s",
	ChatActivityStreamingGeneric:  "Receiving response",
	ChatActivityWorking:           "Working on your request",
	ChatActivityMultiModel:        "Running multi-model evaluation",
	ChatActivityElapsed:           "elapsed %s",
	ChatActivityFallback:          "fallback from %s",
	ChatActivityChunkOne:          "%d chunk",
	ChatActivityChunkMany:         "%d chunks",
	ChatActivityChars:             "%d chars",
	ApprovalTitle:                 "Pending Approval",
	ApprovalPendingLabel:          "Pending approval",
	ApprovalActionHint:            "Use /approve or /deny (or Y/n).",
	ApprovalNone:                  "No pending approval.",
	ApprovalPendingWrite:          "Pending write: %d files",
	ApprovalPlannedCommand:        "Planned command: %s",
	ApprovalDepsConfirm:           "Install dependencies now? This may execute scripts from generated project files. (Y/n)",
	ApprovalTestsConfirm:          "Run project tests now?",
	ApprovalModeLabel:             "Approval mode",
	ApprovalModeManual:            "Manual",
	ApprovalModeSafe:              "Safe",
	ApprovalModeAutopilot:         "Autopilot",
	ApprovalModeChanged:           "Approval mode: %s",
	ApprovalModeHelp:              "/approval [manual|safe|autopilot]",
	ApprovalAutoWrite:             "Safe mode auto-approved writing %d files.",
	ApprovalAutoWriteAutopilot:    "Autopilot applied a verified candidate and wrote %d files.",
	ApprovalAutoDeps:              "Safe mode auto-approved dependency install.",
	ApprovalAutoTests:             "Safe mode auto-approved test execution.",
	ApprovalIsolationUnavailable:  "Sandbox isolation is unavailable (%s), so this command will not run automatically.",
	AutomationCandidateStarted:    "Running multi-provider candidate verification.",
	AutomationCandidateRunning:    "%s generating",
	AutomationCandidateVerifying:  "%s verifying",
	AutomationCandidatePassed:     "%s passed",
	AutomationCandidateRejected:   "%s rejected",
	AutomationCandidateFailed:     "%s failed",
	AutomationCandidateCanceled:   "%s canceled",
	AutomationCandidateSelected:   "Selected %s after verifying %d/%d candidates.",
	AutomationCandidateFallback:   "No candidate passed local verification. Falling back to manual approval.",

	AutomationCandidateIsolationUnavailable: "Candidate code was not executed: %s. Falling back to manual approval.",
	AutomationCandidateWeakVerification:     "Best candidate passed only weak checks (no baseline tests ran). Falling back to manual approval.",
	AutomationCandidateDeletions:            "Candidate deleted files in its workspace (not applied automatically): %s",
	HostCLIExecNotice:                       "Note: the %s CLI ran on this host (in %s) with your environment and credentials — generation is not sandboxed. Treat untrusted repos accordingly (see SECURITY.md).",

	BuildDepsDetectFailed:   "Dependency detection failed: %s",
	BuildTestsDetectFailed:  "Test detection failed: %s",
	BuildDepsExecError:      "Error installing dependencies: %s",
	BuildDepsExecFailed:     "Dependency install failed:\n%s",
	BuildDepsSkipped:        "Skipped dependency install and tests. Run them manually when you're ready.",
	BuildTestsSkipped:       "Skipped tests. Run them manually when you're ready.",
	ExecDepsLabel:           "dependency install",
	ExecTestsLabel:          "tests",
	ExecStarted:             "Running %s",
	ExecFinished:            "%s finished",
	ExecCommand:             "Command: %s",
	ExecExitCode:            "Exit code: %d",
	ExecDuration:            "Duration: %s",
	ExecOutput:              "Output: %s",
	RestoredSessionPrefix:   "Restored previous session",
	RestoredSessionNotice:   "%s (%d messages). Use /clear to start fresh.",
	NoCompactedMemoryNotice: "No compacted memory yet. Conversation is still within the active context window.",
	MemoryRestoredAt:        "%s at %s.",

	ChatHelp: "Available commands:\n" +
		"/help - Show command help\n" +
		"/clear - Clear the current conversation\n" +
		"/compact - Compact older chat history\n" +
		"/memory - Show compacted session memory\n" +
		"/status - Show current session status\n" +
		"/cost - Show current session cost\n" +
		"/approval [manual|safe|autopilot] - Switch approval behavior\n" +
		"/approve - Approve the pending action\n" +
		"/deny - Deny the pending action\n" +
		"/resume - Restore the last saved session\n" +
		"/model [fast|balanced|power] - Switch routing profile\n" +
		"/exit - Quit makewand",
	CompactDone:    "Conversation compacted.",
	CompactNothing: "Nothing to compact yet.",
	ResumeError:    "Could not restore session: %s",
	ResumeNotFound: "No saved session found for this project.",
	IdentityAnswer: "I am **makewand**, a multi-provider AI routing tool.\n\n" +
		"I intelligently route your requests between:\n" +
		"- Claude (Anthropic)\n" +
		"- Gemini (Google)\n" +
		"- Codex (OpenAI)\n\n" +
		"I use adaptive mode-based routing (fast/balanced/power) to choose the best provider for your task.\n\n" +
		"Type /help for available commands, or ask me anything else!",
	ModelProfileLabel:    "Model profile: %s",
	ModeNotSet:           "not set (legacy routing)",
	StatusProvidersNone:  "Available providers: none",
	StatusProviders:      "Available providers: %s",
	StatusProject:        "Project: %s",
	StatusProjectEntries: "Project entries: %d",
	StatusSessionFile:    "Session file: %s",
	StatusLastSaved:      "Last saved: %s",
	StatusSessionCost:    "Session cost: $%.2f",
	Goodbye:              "Goodbye!",

	CostSession:       "Session Cost",
	CostMonth:         "Monthly Total",
	CostFree:          "free",
	CostLocal:         "local",
	CostTotal:         "Total",
	CostSubscription:  "subscription",
	CostRequests:      "%d requests (~%s tokens)",
	CostTokensEst:     "~%dK tokens",
	CostSummaryTotal:  "Session total: $%.2f",
	CostNoRequests:    "No requests yet.",
	CostProviderLine:  "%s: $%.2f, %d requests, %d in / %d out tokens",
	BudgetWarningMsg:  "Budget warning: $%.2f / $%.2f (%.0f%%)",
	BudgetExceededMsg: "Budget exceeded: $%.2f / $%.2f (%.0f%%)",

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

	RepoTrustNoSafeProvider: "Untrusted repository mode is active: only direct API providers may generate against this repo, but none are configured. Set an API key (ANTHROPIC_API_KEY, GEMINI_API_KEY, or OPENAI_API_KEY) to enable a safe provider, or restart with --repo-trust=trusted if you trust this repository.",

	FileTreeTitle: "Project Files",
	FileTreeEmpty: "(no files yet)",
	FileTreeMore:  "+%d more files",

	FileConfirmWrite: "Write %d files to disk? (Y/n)",
	FileCancelled:    "File write cancelled",
	FileWriteCount:   "Wrote %d files",

	FallbackNotice: "%s unavailable, using %s instead",

	ModeFast:     "Fast",
	ModeBalanced: "Balanced",
	ModePower:    "Power",
	ModeLabel:    "Mode",
	ModeChanged:  "Mode: %s",
	ModeHelp:     "/model [fast|balanced|power]",

	SetupWelcome:  "Welcome to makewand! Let's set up your AI models.",
	SetupAPIKey:   "Enter your %s API key (or press Enter to skip):",
	SetupModel:    "Choose your default model:",
	SetupLanguage: "Choose your language:",
	SetupDone:     "Setup complete! Run 'makewand new' to create your first project.",
}

//nolint:gosec // G101 false positive: UI translation strings mention "API key" but contain no credentials.
var zh = Messages{
	AppName:     "makewand",
	AppTagline:  "面向终端开发者的多模型编码路由器",
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

	ChatWelcome:                   "欢迎！我来帮你构建和修改项目。",
	ChatPrompt:                    "你想做什么？",
	ChatCommandHint:               "命令：/model [fast|balanced|power] | /approval [manual|safe|autopilot] | /clear | /status | /cost | /exit（或 Ctrl+D）",
	ChatThinking:                  "正在思考...",
	ChatWorking:                   "正在处理...",
	ChatPlaceholder:               "输入消息... (Enter 发送, / 查看命令)",
	ChatThinkingAnim:              "正在思考...",
	ActivityTitle:                 "当前活动",
	ActivityLabel:                 "当前活动",
	ChatActivityPreparing:         "正在准备请求",
	ChatActivityContext:           "正在收集项目上下文",
	ChatActivitySelectingProvider: "正在选择 %s",
	ChatActivitySelectingGeneric:  "正在选择模型",
	ChatActivityWaitingProvider:   "正在等待 %s 开始响应",
	ChatActivityWaitingGeneric:    "正在等待模型响应",
	ChatActivityStreamingProvider: "正在接收 %s 的输出",
	ChatActivityStreamingGeneric:  "正在接收输出",
	ChatActivityWorking:           "正在处理请求",
	ChatActivityMultiModel:        "正在运行多模型评估",
	ChatActivityElapsed:           "已耗时 %s",
	ChatActivityFallback:          "从 %s 回退",
	ChatActivityChunkOne:          "%d 个片段",
	ChatActivityChunkMany:         "%d 个片段",
	ChatActivityChars:             "%d 字符",
	ApprovalTitle:                 "待确认操作",
	ApprovalPendingLabel:          "待确认操作",
	ApprovalActionHint:            "使用 /approve 或 /deny（或 Y/n）。",
	ApprovalNone:                  "当前没有待确认操作。",
	ApprovalPendingWrite:          "待写入 %d 个文件",
	ApprovalPlannedCommand:        "计划执行命令：%s",
	ApprovalDepsConfirm:           "现在安装依赖吗？这可能会执行生成项目中的脚本。(Y/n)",
	ApprovalTestsConfirm:          "现在运行项目测试吗？",
	ApprovalModeLabel:             "审批模式",
	ApprovalModeManual:            "手动",
	ApprovalModeSafe:              "安全",
	ApprovalModeAutopilot:         "自动驾驶",
	ApprovalModeChanged:           "审批模式：%s",
	ApprovalModeHelp:              "/approval [manual|safe|autopilot]",
	ApprovalAutoWrite:             "安全模式已自动批准写入 %d 个文件。",
	ApprovalAutoWriteAutopilot:    "自动驾驶已应用通过验证的候选，并写入 %d 个文件。",
	ApprovalAutoDeps:              "安全模式已自动批准安装依赖。",
	ApprovalAutoTests:             "安全模式已自动批准运行测试。",
	ApprovalIsolationUnavailable:  "沙箱隔离不可用（%s），该命令不会自动执行。",
	AutomationCandidateStarted:    "正在运行多 provider 候选验证。",
	AutomationCandidateRunning:    "%s 生成中",
	AutomationCandidateVerifying:  "%s 验证中",
	AutomationCandidatePassed:     "%s 已通过",
	AutomationCandidateRejected:   "%s 未通过",
	AutomationCandidateFailed:     "%s 失败",
	AutomationCandidateCanceled:   "%s 已取消",
	AutomationCandidateSelected:   "已选择 %s，通过验证 %d/%d 个候选。",
	AutomationCandidateFallback:   "没有候选通过本地验证，已回退为手动确认。",

	AutomationCandidateIsolationUnavailable: "候选代码未被执行：%s。已回退为手动确认。",
	AutomationCandidateWeakVerification:     "最佳候选只通过了弱校验（没有运行基线测试），已回退为手动确认。",
	AutomationCandidateDeletions:            "候选在其工作区中删除了文件（不会自动应用）：%s",
	HostCLIExecNotice:                       "提示：%s CLI 在本机（%s）以你的环境和凭据运行——生成阶段没有沙箱。处理不可信仓库请注意（详见 SECURITY.md）。",

	BuildDepsDetectFailed:   "依赖检测失败：%s",
	BuildTestsDetectFailed:  "测试检测失败：%s",
	BuildDepsExecError:      "安装依赖时出错：%s",
	BuildDepsExecFailed:     "依赖安装失败：\n%s",
	BuildDepsSkipped:        "已跳过依赖安装和测试。准备好后可手动运行。",
	BuildTestsSkipped:       "已跳过测试。准备好后可手动运行。",
	ExecDepsLabel:           "依赖安装",
	ExecTestsLabel:          "测试",
	ExecStarted:             "正在执行%s",
	ExecFinished:            "%s已完成",
	ExecCommand:             "命令：%s",
	ExecExitCode:            "退出码：%d",
	ExecDuration:            "耗时：%s",
	ExecOutput:              "输出：%s",
	RestoredSessionPrefix:   "已恢复上次会话",
	RestoredSessionNotice:   "%s（%d 条消息）。使用 /clear 可重新开始。",
	NoCompactedMemoryNotice: "还没有压缩记忆。当前对话仍在活动上下文窗口内。",
	MemoryRestoredAt:        "%s，时间：%s。",

	ChatHelp: "可用命令：\n" +
		"/help - 显示命令帮助\n" +
		"/clear - 清空当前对话\n" +
		"/compact - 压缩较早的聊天历史\n" +
		"/memory - 显示压缩后的会话记忆\n" +
		"/status - 显示当前会话状态\n" +
		"/cost - 显示本次会话费用\n" +
		"/approval [manual|safe|autopilot] - 切换审批模式\n" +
		"/approve - 批准待确认操作\n" +
		"/deny - 拒绝待确认操作\n" +
		"/resume - 恢复上次保存的会话\n" +
		"/model [fast|balanced|power] - 切换路由档位\n" +
		"/exit - 退出 makewand",
	CompactDone:    "对话已压缩。",
	CompactNothing: "暂时没有可压缩的内容。",
	ResumeError:    "恢复会话失败：%s",
	ResumeNotFound: "没有找到该项目已保存的会话。",
	IdentityAnswer: "我是 **makewand**，一个多 provider AI 路由工具。\n\n" +
		"我会在以下模型之间智能路由你的请求：\n" +
		"- Claude (Anthropic)\n" +
		"- Gemini (Google)\n" +
		"- Codex (OpenAI)\n\n" +
		"我使用基于档位的自适应路由（fast/balanced/power）来为你的任务选择最合适的 provider。\n\n" +
		"输入 /help 查看可用命令，也可以直接问我任何问题！",
	ModelProfileLabel:    "模型档位：%s",
	ModeNotSet:           "未设置（旧版路由）",
	StatusProvidersNone:  "可用 provider：无",
	StatusProviders:      "可用 provider：%s",
	StatusProject:        "项目：%s",
	StatusProjectEntries: "项目条目：%d 个",
	StatusSessionFile:    "会话文件：%s",
	StatusLastSaved:      "上次保存：%s",
	StatusSessionCost:    "本次会话费用：$%.2f",
	Goodbye:              "再见！",

	CostSession:       "本次费用",
	CostMonth:         "本月累计",
	CostFree:          "免费",
	CostLocal:         "本地",
	CostTotal:         "总计",
	CostSubscription:  "订阅",
	CostRequests:      "%d 次请求 (~%s tokens)",
	CostTokensEst:     "~%dK tokens",
	CostSummaryTotal:  "本次会话合计：$%.2f",
	CostNoRequests:    "还没有任何请求。",
	CostProviderLine:  "%s：$%.2f，%d 次请求，输入 %d / 输出 %d tokens",
	BudgetWarningMsg:  "预算警告：$%.2f / $%.2f (%.0f%%)",
	BudgetExceededMsg: "已超出预算：$%.2f / $%.2f (%.0f%%)",

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

	RepoTrustNoSafeProvider: "已启用不可信仓库模式：只有直连 API 提供方可以对此仓库生成内容，但当前未配置任何一个。请设置 API 密钥（ANTHROPIC_API_KEY、GEMINI_API_KEY 或 OPENAI_API_KEY）以启用安全提供方，或在信任该仓库时用 --repo-trust=trusted 重新启动。",

	FileTreeTitle: "项目文件",
	FileTreeEmpty: "（暂无文件）",
	FileTreeMore:  "还有 %d 个文件",

	FileConfirmWrite: "写入 %d 个文件到磁盘？(Y/n)",
	FileCancelled:    "已取消文件写入",
	FileWriteCount:   "已写入 %d 个文件",

	FallbackNotice: "%s 不可用，已切换到 %s",

	ModeFast:     "快速",
	ModeBalanced: "平衡",
	ModePower:    "强劲",
	ModeLabel:    "模式",
	ModeChanged:  "模式：%s",
	ModeHelp:     "/model [fast|balanced|power]",

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
