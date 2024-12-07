package app

import (
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	filetools "github.com/nuzur/nuzur-cli/file_tools"
	outputtools "github.com/nuzur/nuzur-cli/output_tools"
	"golang.org/x/text/language"
)

func initTranslations() *i18n.Bundle {
	bundle := i18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	translationPath := filepath.Join(filetools.CurrentPath(), "translations")
	bundle.LoadMessageFile(filepath.Join(translationPath, "en.toml"))
	bundle.LoadMessageFile(filepath.Join(translationPath, "es.toml"))
	return bundle
}

func (i *Implementation) Localize(key string, defaultValue string) string {
	localizer := i18n.NewLocalizer(i.i18nBundle, outputtools.GetLocale())
	return localizer.MustLocalize(&i18n.LocalizeConfig{
		MessageID: key,
		DefaultMessage: &i18n.Message{
			ID:    key,
			Other: defaultValue,
		},
	})
}

func (i *Implementation) LocalizeWithVariables(key string, variables map[string]string, defaultValue string) string {
	localizer := i18n.NewLocalizer(i.i18nBundle, outputtools.GetLocale())
	return localizer.MustLocalize(&i18n.LocalizeConfig{
		MessageID:    key,
		TemplateData: variables,
		DefaultMessage: &i18n.Message{
			ID:    key,
			Other: defaultValue,
		},
	})
}
