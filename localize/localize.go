package localize

import (
	"path/filepath"

	_ "embed"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/nuzur/filetools"
	"github.com/nuzur/nuzur-cli/outputtools"
	"golang.org/x/text/language"
)

type Implementation struct {
	bundle *i18n.Bundle
}

//go:embed translations/en.toml
var translations_en string

//go:embed translations/es.toml
var translations_es string

func New() *Implementation {

	bundle := i18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)

	translations_en_path := filepath.Join("/tmp", "nuzur", "en.toml")
	translations_es_path := filepath.Join("/tmp", "nuzur", "es.toml")
	filetools.Write(translations_en_path, []byte(translations_en))
	filetools.Write(translations_es_path, []byte(translations_es))

	bundle.LoadMessageFile(translations_en_path)
	bundle.LoadMessageFile(translations_es_path)
	return &Implementation{
		bundle: bundle,
	}
}

func (i *Implementation) Localize(key string, defaultValue string) string {
	localizer := i18n.NewLocalizer(i.bundle, outputtools.GetLocale())
	return localizer.MustLocalize(&i18n.LocalizeConfig{
		MessageID: key,
		DefaultMessage: &i18n.Message{
			ID:    key,
			Other: defaultValue,
		},
	})
}

func (i *Implementation) LocalizeWithVariables(key string, variables map[string]string, defaultValue string) string {
	localizer := i18n.NewLocalizer(i.bundle, outputtools.GetLocale())
	return localizer.MustLocalize(&i18n.LocalizeConfig{
		MessageID:    key,
		TemplateData: variables,
		DefaultMessage: &i18n.Message{
			ID:    key,
			Other: defaultValue,
		},
	})
}
