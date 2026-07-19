// Binary service-python is the generic Python agent entry point.
//
// All logic lives in subpackages under ./pkg so specializations
// (python-fastapi, python-grpc, …) can import and compose them:
//
//	github.com/codefly-dev/service-python/pkg/service   — shared Service/Settings
//	github.com/codefly-dev/service-python/pkg/runtime   — Runtime gRPC server
//	github.com/codefly-dev/service-python/pkg/code      — Code gRPC server
//	github.com/codefly-dev/service-python/pkg/tooling   — Tooling gRPC server
//	github.com/codefly-dev/service-python/pkg/builder   — Builder gRPC server
//
// A specialization's main.go embeds these types (via Go struct embedding)
// and overrides only what its layer adds.
package main

import (
	"embed"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/toolbox/lang"

	pythonbuilder "github.com/codefly-dev/service-python/pkg/builder"
	pythoncode "github.com/codefly-dev/service-python/pkg/code"
	pythonruntime "github.com/codefly-dev/service-python/pkg/runtime"
	pythonservice "github.com/codefly-dev/service-python/pkg/service"
	pythontooling "github.com/codefly-dev/service-python/pkg/tooling"
)

//go:embed agent.codefly.yaml
var infoFS embed.FS

var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

func main() {
	svc := pythonservice.New(agent.Of(resources.ServiceAgent))
	code := pythoncode.New(svc)
	runtime := pythonruntime.New(svc)
	tooling := pythontooling.New(code, runtime)
	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: runtime,
		Builder: pythonbuilder.New(svc),
		Code:    code,
		Tooling: tooling,
		Toolbox: lang.NewToolboxFromTooling(agent.Name, agent.Version, tooling),
	})
}
