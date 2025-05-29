# Go MCP SSE Proxy

> **⚠️ ARCHIVED PROJECT**
>
> This project has been archived and is no longer maintained due to persistent stability and reliability issues that made it unsuitable for production use. The proxy exhibited unpredictable behavior under load, leading to resource exhaustion and service interruptions that could not be adequately resolved.
>
> **I recommend using my [TypeScript MCP SSE Proxy](https://github.com/danpiths/ts-mcp-sse-proxy) as an alternative to this project.**

A high-performance Server-Sent Events (SSE) proxy server designed for the Model Context Protocol (MCP), built in Go. This proxy enables efficient real-time communication between MCP clients and servers while providing rate limiting, security features, and process management.

## Features

- 🚀 High-performance SSE proxy for MCP communication
- 🔒 Built-in security headers and authentication middleware
- ⚡ Rate limiting with configurable thresholds
- 🔄 Process management for handling MCP server instances
- 📝 Configurable logging levels
- 🛡️ Graceful shutdown handling
- 🐳 Docker support with multi-stage builds
- 🚀 Fly.io deployment configuration

## Prerequisites

- Go 1.24.2 or higher
- Docker (for containerized deployment)
- Node.js and npm (for supergateway dependency)

## Installation

### Local Development

1. Clone the repository:

```bash
git clone https://github.com/danpiths/go-mcp-sse-proxy.git
cd go-mcp-sse-proxy
```

2. Install dependencies:

```bash
go mod download
```

3. Build the project:

```bash
go build -o out ./cmd/proxy
```

### Docker Deployment

Build the Docker image:

```bash
docker build -t go-mcp-sse-proxy .
```

Run the container:

```bash
docker run -p 8000:8000 go-mcp-sse-proxy
```

## Configuration

### Environment Variables

- `LOG_LEVEL`: Set logging level (DEBUG, INFO, WARN, ERROR)
- `MAXIM_SECRET`: Required secret key for authentication
- Additional environment variables can be configured for authentication and rate limiting

### Rate Limiting

The default rate limit is set to:

- 100 requests per minute
- Burst limit of 10 requests

You can modify these values in `cmd/proxy/main.go`:

```go
rateLimiter := ratelimit.NewRateLimiter(rate.Limit(100.0/60.0), 10)
```

### Process Management

The process manager handles MCP server instances with configurable settings in `internal/config/config.go`.

### Command Execution Security

To prevent remote code execution, the proxy implements strict command whitelisting:

- Only pre-approved commands in `internal/config/config.go` can be executed
- Environment variables are protected with a blacklist to prevent overwriting sensitive data
- Each command must be explicitly allowed in the `AllowedCommands` map

### Adding New Commands

To add support for new commands:

1. Open `internal/config/config.go`
2. Add your command to the `AllowedCommands` map:

```go
var AllowedCommands = map[string]string{
    "your-command-string": "your-command-string",
    // ... existing commands ...
}
```

Note: The command string must be exact and both key and value should be identical.

## API Endpoints

This proxy implements the standard MCP SSE protocol with an additional authorization header check for security. The `/health` endpoint is available for monitoring service status.

### Connecting to MCP Servers

Here's how to connect to the proxy server once it is deployed:

1. Set up environment variables (if required):

   - Add any required environment variables with the `ENV_` prefix in your request headers
   - Example: For GitHub MCP server, set `ENV_GITHUB_PERSONAL_ACCESS_TOKEN` header

2. Make SSE connection:
   - URL format: `https://<your-domain>/<encoded-command>/sse`
   - Required headers:
     ```
     Authorization: Bearer <your-MAXIM_SECRET>
     ```

#### Example: Context7 MCP Server

Using the official MCP TypeScript SDK:

```typescript
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { SSEClientTransport } from "@modelcontextprotocol/sdk/client/sse.js";

const client = new Client({
  name: "example-client",
  version: "1.0.0",
});

const transport = new SSEClientTransport(new URL("https://<your-domain>/npx%20-y%20%40upstash%2Fcontext7-mcp%40latest/sse"), {
  requestInit: {
    headers: {
      Authorization: `Bearer ${process.env.MAXIM_SECRET}`,
    },
  },
  eventSourceInit: {
    fetch: (url, init) => {
      return fetch(url, {
        ...init,
        headers: init?.headers
          ? {
              ...(init?.headers ?? {}),
              ...{
                Authorization: `Bearer ${process.env.MAXIM_SECRET}`,
              },
            }
          : {
              Authorization: `Bearer ${process.env.MAXIM_SECRET}`,
            },
      });
    },
  },
});

await client.connect(transport);
```

Note: The Context7 MCP server doesn't require any additional environment variables.

## Deployment

### Fly.io Deployment

The project includes Fly.io configuration. To deploy:

1. First-time setup only:

```bash
fly launch  # Important: Select 'Y' when asked to copy the configuration (fly.toml) to the new app
```

2. For all deployments:

```bash
./deploy.sh
```

## Development

### Project Structure

```
.
├── cmd/
│   └── proxy/          # Main application entry point
├── internal/
│   ├── config/         # Configuration management
│   ├── handlers/       # HTTP handlers and middleware
│   ├── process/        # Process management
│   └── ratelimit/      # Rate limiting implementation
├── pkg/
│   └── logger/         # Logging utilities
├── Dockerfile          # Multi-stage Docker build
├── fly.toml            # Fly.io deployment configuration
└── go.mod             # Go module definition
```

### Contributing

1. Fork the repository
2. Create your feature branch
3. Commit your changes
4. Push to the branch
5. Create a new Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- [Model Context Protocol](https://modelcontextprotocol.io) - For the standardized protocol that enables secure AI tool interactions
- [Supergateway](https://github.com/supercorp-ai/supergateway) - For providing the essential MCP stdio to SSE conversion functionality
- [Charm](https://charm.sh/) - For the dope TUI libraries (used for logging in this application)
- [Fly.io](https://fly.io) - For hosting and deployment infrastructure
- [Go Time Rate](https://pkg.go.dev/golang.org/x/time/rate) - For rate limiting implementation
