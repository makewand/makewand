// Package privatefile protects an already opened, regular, single-link file.
// Callers must open the intended leaf without following symlinks or reparse
// points and enforce their own ancestor/path policy. No pathname is reopened.
package privatefile
