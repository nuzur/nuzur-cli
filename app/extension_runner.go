package app

import (
	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/extensionrun"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// extensionRunner is everything package app asks of the extension-run client.
//
// It is a CONSUMER-side interface: the list below is exactly the set of methods
// this package calls on *extensionrun.Implementation, no more, and it is declared
// here rather than in extensionrun because the dependency runs this way — app
// needs a seam, extensionrun does not need to know it has one.
//
// The point is testability of the deploy pipeline. Every remote thing a deploy
// does that is not SSH or a provider CLI goes through this client: resolving the
// project and version, building and validating the generator config, running the
// generator, and running sql-push (which is how the schema reaches the database,
// confirmation step and all). Without a seam here the pipeline cannot be driven
// at all without the live extension server.
//
// The compile-time assertion is what keeps the list honest: it is generated from
// the concrete type's real signatures, and any drift is a build failure.
var _ extensionRunner = (*extensionrun.Implementation)(nil)

type extensionRunner interface {
	// Discovery: projects, versions, extensions.
	ListUserProjects() ([]*nemgen.Project, error)
	ListProjectVersions(projectUUID string) ([]*nemgen.ProjectVersion, error)
	ListGeneratorExtensions() ([]*nemgen.Extension, error)
	ListRunnableExtensions(pairFronts []string) ([]*nemgen.Extension, error)
	FindExtensionByIdentifier(identifier string) (*nemgen.Extension, error)
	GetLatestExtensionVersion(extensionUUID string) (*nemgen.ExtensionVersion, error)
	GetConfigEntity(extensionVersion *nemgen.ExtensionVersion) (*extensiongen.ExtensionConfigurationEntity, error)

	// Access + limits.
	GetUserRoleForProject(projectUUID string) (nemgen.UserProjectRole, error)
	CheckExtensionExecutionLimit(projectUUID string, extensionUUID string) (*pb.CheckExtensionExecutionLimitResponse, error)

	// Config: the saved config, the schema for it, and the two ways of building
	// one (interactively via the resolver, or from JSON).
	GetLastUsedConfigs(projectVersionUUID string) (map[string]extensionrun.LastUsedEntry, error)
	SaveLastUsedConfigEntry(projectVersionUUID, extensionIdentifier string, configValues map[string]interface{}) error
	NewConfigResolver(project *nemgen.Project, projectVersionUUID string) *extensionrun.ConfigResolver
	DescribeConfig(
		project *nemgen.Project,
		projectVersion *nemgen.ProjectVersion,
		extension *nemgen.Extension,
		extensionVersion *nemgen.ExtensionVersion,
		configEntity *extensiongen.ExtensionConfigurationEntity,
		lastConfig map[string]interface{},
	) (*extensionrun.ConfigSchema, error)
	BuildConfigFromJSON(
		project *nemgen.Project,
		projectVersionUUID string,
		configEntity *extensiongen.ExtensionConfigurationEntity,
		provided map[string]interface{},
		lastConfig map[string]interface{},
	) (map[string]interface{}, error)

	// Schema facts a deploy reads directly (the create-plan path).
	GetStandaloneEntities(projectVersionUUID string) ([]*nemgen.Entity, error)
	ValidateJWTAuthRequirements(projectVersionUUID string, configValues map[string]interface{}) (*extensionrun.ConfigValidationError, []string, error)

	// The execution itself.
	Run(params extensionrun.RunParams) (*extensionrun.RunResult, error)
}
