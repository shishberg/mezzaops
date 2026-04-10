# MezzaOps

## Development workflow

- **TDD**: Always write failing tests first, then implement the fix/feature to make them pass.
- **Delegate implementation**: Use subagents for implementation tasks.
- **Run `make check` before committing**: Always run `make check` and fix any issues before creating a commit.
- **Formatting**: Run `gofmt -w .` to fix formatting issues rather than editing by hand.
- **Commit often, push once**: Commit after every completed implementation task. Push only after all tasks are done.
