package template

// Template defines a project template.
type Template struct {
	ID          string
	Name        string
	NameZh      string
	Icon        string
	Description string
	DescZh      string
	TechStack   string
	Prompt      string // system prompt for AI to generate this type of project
}

// All returns all available templates.
func All() []Template {
	return []Template{
		{
			ID:          "blog",
			Name:        "Personal Blog / Portfolio",
			NameZh:      "个人博客 / 作品集",
			Icon:        "📝",
			Description: "A personal blog or portfolio website with posts and pages",
			DescZh:      "个人博客或作品集网站，支持文章和页面",
			TechStack:   "React + Tailwind CSS",
			Prompt: `Create a personal blog/portfolio website. Requirements:
- Modern, responsive design using React and Tailwind CSS
- Home page with introduction
- Blog section with markdown posts
- Portfolio/projects section
- Contact page
- Dark/light mode toggle
- SEO-friendly
Generate a complete, working project with package.json, all components, and sample content.`,
		},
		{
			ID:          "ecommerce",
			Name:        "E-commerce Store",
			NameZh:      "电商网站",
			Icon:        "🛒",
			Description: "An online store with product listings, cart, and checkout",
			DescZh:      "在线商店，支持商品展示、购物车和结账",
			TechStack:   "React + FastAPI + SQLite",
			Prompt: `Create an e-commerce store. Requirements:
- Product listing with categories and search
- Product detail pages with images
- Shopping cart functionality
- Simple checkout flow
- Admin panel for managing products
- React frontend with Tailwind CSS
- FastAPI backend with SQLite database
Generate a complete, working project with both frontend and backend.`,
		},
		{
			ID:          "dashboard",
			Name:        "Data Dashboard",
			NameZh:      "数据看板",
			Icon:        "📊",
			Description: "An interactive dashboard with charts and data visualization",
			DescZh:      "交互式数据看板，带图表和数据可视化",
			TechStack:   "React + Recharts + FastAPI",
			Prompt: `Create a data dashboard. Requirements:
- Multiple chart types (line, bar, pie, area)
- Responsive grid layout
- Date range filters
- Data tables with sorting and pagination
- Export to CSV
- React frontend with Recharts
- FastAPI backend with sample data
Generate a complete, working project.`,
		},
		{
			ID:          "chatbot",
			Name:        "Chatbot",
			NameZh:      "聊天机器人",
			Icon:        "🤖",
			Description: "A conversational chatbot with AI integration",
			DescZh:      "AI 聊天机器人",
			TechStack:   "React + FastAPI + LLM API",
			Prompt: `Create a chatbot application. Requirements:
- Chat interface with message bubbles
- Typing indicator
- Message history
- Support for multiple conversations
- FastAPI backend with OpenAI-compatible API integration
- Clean, modern UI
Generate a complete, working project.`,
		},
		{
			ID:          "script",
			Name:        "Automation Script",
			NameZh:      "自动化脚本",
			Icon:        "🔧",
			Description: "A Python automation script with CLI interface",
			DescZh:      "Python 自动化脚本，带命令行界面",
			TechStack:   "Python + Click",
			Prompt: `Create a Python automation script. Requirements:
- CLI interface with Click
- Configuration file support (YAML)
- Logging
- Progress bars
- Error handling with helpful messages
- Modular structure
Generate a complete, working project with setup.py and requirements.txt.`,
		},
	}
}

// Get returns a template by ID.
func Get(id string) *Template {
	for _, t := range All() {
		if t.ID == id {
			return &t
		}
	}
	return nil
}
