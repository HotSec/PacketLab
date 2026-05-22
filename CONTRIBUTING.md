# Contributing

Thanks for considering contributing to PacketLab!

## Development Setup

```bash
git clone https://github.com/user/packetlab.git
cd packetlab
go mod download
go build -o packetlab ./cmd/proxy/
```

## Code Style

- `gofmt` before commit
- Feature branches from `main`
- Commit messages: `type: description`
  - `feat:` new feature
  - `fix:` bug fix
  - `refactor:` code change without feature/bug
  - `docs:` documentation only
  - `style:` formatting, semicolons, etc.

## Project Structure

```
cmd/proxy/main.go           # Entry point
cmd/proxy/web/              # Embedded frontend (SPA)
internal/proxy/             # Proxy engine (HTTP/HTTPS, MITM, intercept)
internal/api/               # REST API + WebSocket
internal/store/             # SQLite persistence
internal/models/            # Shared data models
internal/config/            # Configuration
```

## Testing

```bash
# Run all tests
go test ./...

# Run specific package
go test ./internal/proxy/ -v

# Start dev server (no proxy)
go run ./cmd/proxy/ --no-proxy --no-mitm --api-port 9090
```

## Pull Request Checklist

- [ ] Code compiles: `go build ./cmd/proxy/`
- [ ] Formatted: `gofmt -s -w .`
- [ ] No regressions: manual smoke test
- [ ] Commit message follows convention

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
