# OS with Go

Learn core operating systems concepts through the Go runtime

This is a mini course intended for anyone with some programming background who wants to better understand systems in general. It takes typical concepts from an operating systems class and teaches them in a modern context through the Go runtime. The Go source code is quite readable, so we'll be able to dive into the real implementations of these concepts in a real and widely used system.

> [!WARNING]
> This is still at an early stage, and was mostly written by Claude. I plan to review and refine it. The target is a self-contained, ~10 hour course, with additional optional material.

### Building

Install [mdbook](https://rust-lang.github.io/mdBook/guide/installation.html).

```
mdbook serve
mdbook build
```

### Maintaining source links

Code snippets include citations to the Go runtime source with links to specific line numbers. When updating to a new Go version, use the verification tool to check and fix line numbers:

```
go run ./tools/verify-lines -go-root ~/sw/go       # check citations
go run ./tools/verify-lines -go-root ~/sw/go -fix   # auto-fix shifted lines
```

The tool reads the `VERSION` file from the Go source tree to set the tag in URLs. Use `-tag go1.XX` to override.
