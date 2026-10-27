package codescan

import (
	"go/ast"
	"go/token"
	"net"
	"net/url"
	"strconv"
	"strings"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"golang.org/x/tools/go/packages"
)

// externalCatalog maps SDK import-path prefixes to the external system they
// talk to. Longer prefixes are listed before shorter ones they overlap.
var externalCatalog = []struct{ prefix, system string }{
	{"github.com/redis/go-redis", "redis"},
	{"github.com/go-redis/redis", "redis"},
	{"github.com/gomodule/redigo", "redis"},
	{"github.com/aws/aws-sdk-go-v2/service/s3", "aws-s3"},
	{"github.com/aws/aws-sdk-go/service/s3", "aws-s3"},
	{"github.com/aws/aws-sdk-go-v2/service/sesv2", "aws-ses"},
	{"github.com/aws/aws-sdk-go-v2/service/ses", "aws-ses"},
	{"github.com/aws/aws-sdk-go/service/ses", "aws-ses"},
	{"github.com/aws/aws-sdk-go-v2/service/sqs", "aws-sqs"},
	{"github.com/aws/aws-sdk-go/service/sqs", "aws-sqs"},
	{"github.com/aws/aws-sdk-go-v2/service/sns", "aws-sns"},
	{"github.com/aws/aws-sdk-go/service/sns", "aws-sns"},
	{"github.com/aws/aws-sdk-go-v2/service/dynamodb", "aws-dynamodb"},
	{"github.com/aws/aws-sdk-go/service/dynamodb", "aws-dynamodb"},
	{"github.com/aws/aws-sdk-go-v2/service/kms", "aws-kms"},
	{"github.com/aws/aws-sdk-go/service/kms", "aws-kms"},
	{"github.com/stripe/stripe-go", "stripe"},
	{"github.com/zmb3/spotify", "spotify"},
	{"github.com/anthropics/anthropic-sdk-go", "anthropic"},
	{"github.com/openai/openai-go", "openai"},
	{"github.com/sashabaranov/go-openai", "openai"},
	{"github.com/go-sql-driver/mysql", "mysql"},
	{"github.com/lib/pq", "postgres"},
	{"github.com/jackc/pgx", "postgres"},
	{"go.mongodb.org/mongo-driver", "mongodb"},
	{"cloud.google.com/go/storage", "gcs"},
	{"cloud.google.com/go/pubsub", "gcp-pubsub"},
	{"firebase.google.com/go", "firebase"},
	{"github.com/slack-go/slack", "slack"},
	{"github.com/bwmarrin/discordgo", "discord"},
	{"github.com/sendgrid/sendgrid-go", "sendgrid"},
	{"github.com/twilio/twilio-go", "twilio"},
	{"github.com/getsentry/sentry-go", "sentry"},
	{"github.com/nats-io/nats.go", "nats"},
	{"github.com/segmentio/kafka-go", "kafka"},
	{"github.com/IBM/sarama", "kafka"},
	{"github.com/rabbitmq/amqp091-go", "rabbitmq"},
	{"github.com/elastic/go-elasticsearch", "elasticsearch"},
	{"github.com/meilisearch/meilisearch-go", "meilisearch"},
}

func externalForImport(path string) (string, bool) {
	for _, e := range externalCatalog {
		// Subpackages and major-version suffixes (stripe-go/v82) both match.
		if path == e.prefix || strings.HasPrefix(path, e.prefix+"/") {
			return e.system, true
		}
	}
	return "", false
}

func isRedisSDK(path string) bool {
	sys, ok := externalForImport(path)
	return ok && sys == "redis"
}

// ignoredHosts are hosts that appear in code as identifiers, not as systems
// the code talks to (XML namespaces, schema ids, documentation examples).
var ignoredHosts = map[string]bool{
	"localhost": true, "www.w3.org": true, "w3.org": true, "schema.org": true,
	"json-schema.org": true, "purl.org": true, "example.com": true, "example.org": true,
	"www.example.com": true, "golang.org": true, "go.dev": true, "pkg.go.dev": true,
}

// hostLiteral turns an http(s) URL string literal into an external host.
func (s *scanner) hostLiteral(p *packages.Package, lit *ast.BasicLit) {
	if lit.Kind != token.STRING {
		return
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil || !(strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) {
		return
	}
	u, err := url.Parse(v)
	if err != nil {
		return
	}
	s.addHost(p, u.Hostname(), "url", lit.Pos())
}

// dialCall records gRPC dial targets (grpc.Dial("api.example.com:443")).
func (s *scanner) dialCall(p *packages.Package, call *ast.CallExpr) {
	fn := calledFunc(p.TypesInfo, call)
	if fn == nil || funcPkgPath(fn) != "google.golang.org/grpc" {
		return
	}
	idx := -1
	switch fn.Name() {
	case "Dial", "NewClient":
		idx = 0
	case "DialContext":
		idx = 1
	}
	if idx < 0 || idx >= len(call.Args) {
		return
	}
	target, ok := stringConst(p.TypesInfo, call.Args[idx])
	if !ok {
		return
	}
	target = strings.TrimPrefix(strings.TrimPrefix(target, "dns:///"), "passthrough:///")
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	s.addHost(p, host, "grpc", call.Pos())
}

func (s *scanner) addHost(p *packages.Package, host, via string, pos token.Pos) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || !strings.Contains(host, ".") || ignoredHosts[host] ||
		strings.ContainsAny(host, "%{}$<>") || net.ParseIP(host) != nil ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".example.com") || strings.HasSuffix(host, ".svc.cluster.local") {
		return
	}
	id := "ext:" + host
	n := s.b.node(id, pb.CodeNodeKind_CODE_NODE_KIND_EXTERNAL_SYSTEM, host)
	if n.Attrs == nil {
		n.Attrs = map[string]string{"via": via}
	} else if n.Attrs["via"] != via && !strings.Contains(n.Attrs["via"], via) {
		n.Attrs["via"] = mergeVia(n.Attrs["via"], via)
	}
	s.b.edge(pb.CodeEdgeKind_CODE_EDGE_KIND_USES_EXTERNAL, p.PkgPath, id, s.fset.Position(pos), "")
}

func mergeVia(a, b string) string {
	if a < b {
		return a + "," + b
	}
	return b + "," + a
}
