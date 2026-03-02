package i18n

// T translates a key to the current language.
var currentLang = "en"

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
	WizardWelcome    string
	WizardPrompt     string
	WizardPlanning   string
	WizardConfirm    string
	WizardBuilding   string
	WizardDone       string
	WizardTemplate   string
	WizardCustom     string

	// Templates
	TplBlog      string
	TplEcommerce string
	TplDashboard string
	TplChatbot   string
	TplMiniApp   string
	TplScript    string

	// Chat
	ChatWelcome  string
	ChatPrompt   string
	ChatThinking string
	ChatWorking  string

	// Cost
	CostSession string
	CostMonth   string
	CostFree    string
	CostLocal   string
	CostTotal   string

	// Progress
	ProgressAnalyzing string
	ProgressCreating  string
	ProgressTesting   string
	ProgressFixing    string
	ProgressDone      string

	// Errors
	ErrNoModel    string
	ErrNoAPIKey   string
	ErrBuildFail  string
	ErrTestFail   string
	ErrAutofix    string

	// File tree
	FileTreeTitle string

	// Setup
	SetupWelcome  string
	SetupAPIKey   string
	SetupModel    string
	SetupLanguage string
	SetupDone     string
}

var en = Messages{
	AppName:    "makewand",
	AppTagline: "AI coding assistant for everyone",
	Version:    "v0.1.0",
	Yes:        "Yes",
	No:         "No",
	Confirm:    "Confirm",
	Cancel:     "Cancel",
	Back:       "Back",
	Next:       "Next",
	Done:       "Done",
	Error:      "Error",
	Warning:    "Warning",
	Quit:       "Quit",
	QuitConfirm: "Are you sure you want to quit?",

	WizardWelcome:  "What would you like to build?",
	WizardPrompt:   "Describe your project in plain language:",
	WizardPlanning: "Let me plan this for you...",
	WizardConfirm:  "Ready to start building?",
	WizardBuilding: "Building your project...",
	WizardDone:     "Your project is ready!",
	WizardTemplate: "Pick a template to start:",
	WizardCustom:   "Describe in your own words...",

	TplBlog:      "Personal Blog / Portfolio",
	TplEcommerce: "E-commerce Store",
	TplDashboard: "Data Dashboard",
	TplChatbot:   "Chatbot",
	TplMiniApp:   "Mobile App",
	TplScript:    "Automation Script",

	ChatWelcome:  "Welcome! I'm here to help you build and modify your project.",
	ChatPrompt:   "What would you like to do?",
	ChatThinking: "Thinking...",
	ChatWorking:  "Working on it...",

	CostSession: "Session Cost",
	CostMonth:   "Monthly Total",
	CostFree:    "free",
	CostLocal:   "local",
	CostTotal:   "Total",

	ProgressAnalyzing: "Analyzing requirements...",
	ProgressCreating:  "Creating files...",
	ProgressTesting:   "Running tests...",
	ProgressFixing:    "Fixing issues...",
	ProgressDone:      "All done!",

	ErrNoModel:  "No AI model configured. Run 'makewand setup' first.",
	ErrNoAPIKey: "API key not set for %s. Set it with 'makewand setup' or the environment variable.",
	ErrBuildFail: "Build failed",
	ErrTestFail:  "Tests failed",
	ErrAutofix:   "Don't worry! Let me analyze and fix this...",

	FileTreeTitle: "Project Files",

	SetupWelcome:  "Welcome to makewand! Let's set up your AI models.",
	SetupAPIKey:   "Enter your %s API key (or press Enter to skip):",
	SetupModel:    "Choose your default model:",
	SetupLanguage: "Choose your language:",
	SetupDone:     "Setup complete! Run 'makewand new' to create your first project.",
}

var zh = Messages{
	AppName:    "makewand",
	AppTagline: "人人都能用的 AI 编程助手",
	Version:    "v0.1.0",
	Yes:        "是",
	No:         "否",
	Confirm:    "确认",
	Cancel:     "取消",
	Back:       "返回",
	Next:       "下一步",
	Done:       "完成",
	Error:      "错误",
	Warning:    "警告",
	Quit:       "退出",
	QuitConfirm: "确定要退出吗？",

	WizardWelcome:  "你想构建什么？",
	WizardPrompt:   "用自然语言描述你的项目：",
	WizardPlanning: "我来帮你规划...",
	WizardConfirm:  "确认开始构建？",
	WizardBuilding: "正在构建你的项目...",
	WizardDone:     "项目已就绪！",
	WizardTemplate: "选择一个模板开始：",
	WizardCustom:   "用自己的话描述...",

	TplBlog:      "个人博客 / 作品集",
	TplEcommerce: "电商网站",
	TplDashboard: "数据看板",
	TplChatbot:   "聊天机器人",
	TplMiniApp:   "小程序 / 移动端",
	TplScript:    "自动化脚本",

	ChatWelcome:  "欢迎！我来帮你构建和修改项目。",
	ChatPrompt:   "你想做什么？",
	ChatThinking: "正在思考...",
	ChatWorking:  "正在处理...",

	CostSession: "本次费用",
	CostMonth:   "本月累计",
	CostFree:    "免费",
	CostLocal:   "本地",
	CostTotal:   "总计",

	ProgressAnalyzing: "正在分析需求...",
	ProgressCreating:  "正在创建文件...",
	ProgressTesting:   "正在运行测试...",
	ProgressFixing:    "正在修复问题...",
	ProgressDone:      "全部完成！",

	ErrNoModel:  "未配置 AI 模型。请先运行 'makewand setup'。",
	ErrNoAPIKey: "未设置 %s 的 API 密钥。用 'makewand setup' 或环境变量设置。",
	ErrBuildFail: "构建失败",
	ErrTestFail:  "测试失败",
	ErrAutofix:   "不要慌！让我来分析和修复...",

	FileTreeTitle: "项目文件",

	SetupWelcome:  "欢迎使用 makewand！让我们配置 AI 模型。",
	SetupAPIKey:   "输入你的 %s API 密钥（按回车跳过）：",
	SetupModel:    "选择默认模型：",
	SetupLanguage: "选择语言：",
	SetupDone:     "配置完成！运行 'makewand new' 来创建你的第一个项目。",
}

// SetLanguage sets the current language.
func SetLanguage(lang string) {
	if lang == "zh" || lang == "en" {
		currentLang = lang
	}
}

// GetLanguage returns the current language.
func GetLanguage() string {
	return currentLang
}

// Msg returns the messages for the current language.
func Msg() *Messages {
	if currentLang == "zh" {
		return &zh
	}
	return &en
}
