module github.com/AgentTown/agenttown-mcp/wsserver

go 1.25.0

require (
	github.com/AgentTown/agenttown-mcp/contract v0.0.0
	github.com/coder/websocket v1.8.15
	github.com/google/uuid v1.6.0
)

replace github.com/AgentTown/agenttown-mcp/contract => ../contract
