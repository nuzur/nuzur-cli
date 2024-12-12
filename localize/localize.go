package localize

import (
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/nuzur/filetools"
	"github.com/nuzur/nuzur-cli/outputtools"
	"golang.org/x/text/language"
)

type Implementation struct {
	bundle *i18n.Bundle
}

func New() *Implementation {
	bundle := i18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	translationPath := filepath.Join(filetools.CurrentPath(), "translations")
	bundle.LoadMessageFile(filepath.Join(translationPath, "en.toml"))
	bundle.LoadMessageFile(filepath.Join(translationPath, "es.toml"))
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
