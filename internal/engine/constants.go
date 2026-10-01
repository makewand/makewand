package engine

// ProjectIgnoreDirs contains canonical directories to ignore during repository
// traversal, project scanning, manifest generation, and workspace operations.
var ProjectIgnoreDirs = map[string]bool{
	".git":                   true,
	"__pycache__":            true,
	"node_modules":           true,
	"vendor":                 true,
	"target":                 true,
	"dist":                   true,
	"build":                  true,
	".pytest_cache":          true,
	".mypy_cache":            true,
	".ruff_cache":            true,
	".venv":                  true,
	"venv":                   true,
	"env":                    true,
	".coverage":              true,
	".tox":                   true,
	".idea":                  true,
	".vscode":                true,
	"site-packages":          true,
	".makewand_sandbox_home": true,
	".DS_Store":              true,
	".makewand":              true,
}

// IsProjectIgnoredDir reports whether a directory or path name should be ignored during traversal.
func IsProjectIgnoredDir(name string) bool {
	return ProjectIgnoreDirs[name]
}
