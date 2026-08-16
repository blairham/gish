package p10k

import (
	"path/filepath"
	"strings"
)

// Cloud and cluster context segments.
//
// These are the segments that exist to stop you running the right
// command against the wrong account, so correctness matters more here
// than anywhere else in the prompt: it is better to show nothing than to
// show a context that is not the one your next command will use.
//
// All of them read the same configuration files the tools themselves
// read. None of them shells out to `kubectl config current-context` or
// `aws configure get` — those cost tens of milliseconds each and there
// are usually several on a line, which is how a prompt ends up with a
// visible lag on every keystroke.

func init() {
	register("kubecontext", renderKubecontext)
	register("aws", renderAWS)
	register("aws_eb_env", renderAWSEBEnv)
	register("azure", renderAzure)
	register("gcloud", renderGcloud)
	register("google_app_cred", renderGoogleAppCred)
	register("toolbox", renderToolbox)
}

// renderKubecontext shows the current kubernetes context.
//
// The context is one line in a YAML file, and finding it does not need a
// YAML parser: "current-context:" appears at the top level, so the first
// unindented occurrence is the answer. A real parser here would mean
// pulling a dependency into the prompt path to read one scalar.
func renderKubecontext(cfg *Config, ctx *Context) (Rendered, bool) {
	path := ctx.Env("KUBECONFIG")
	if path != "" {
		// KUBECONFIG is a path list; the first entry wins for reads.
		path, _, _ = strings.Cut(path, string(filepath.ListSeparator))
	} else {
		path = filepath.Join(ctx.Home, ".kube", "config")
	}
	context := yamlTopLevel(readFile(path), "current-context")
	if context == "" {
		return Rendered{}, false
	}

	// Contexts from cloud providers are long and mostly boilerplate; the
	// distinguishing part is at the end.
	if max := cfg.ParamInt("kubecontext", "", "MAX_LENGTH", 32); max > 0 && len(context) > max {
		context = "…" + context[len(context)-max:]
	}
	if ns := ctx.Env("KUBE_NAMESPACE"); ns != "" && ns != "default" {
		context += "/" + ns
	}

	// A context whose name looks like production colors itself as such:
	// this segment earns its place at exactly that moment.
	state := "DEFAULT"
	for _, danger := range []string{"prod", "production", "live"} {
		if strings.Contains(strings.ToLower(context), danger) {
			state = "PROD"
			break
		}
	}
	return Rendered{
		Content: context,
		State:   state,
		Icon:    decodeEscapes(cfg.Param("kubecontext", state, "VISUAL_IDENTIFIER_EXPANSION", "☸")),
	}, true
}

// yamlTopLevel finds a top-level scalar in a YAML document without
// parsing it. Only unindented keys count, so a nested key of the same
// name cannot be mistaken for the real one.
func yamlTopLevel(doc, key string) string {
	for line := range strings.SplitSeq(doc, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// renderAWS shows the active profile and region.
func renderAWS(cfg *Config, ctx *Context) (Rendered, bool) {
	profile := ctx.Env("AWS_PROFILE")
	if profile == "" {
		profile = ctx.Env("AWS_DEFAULT_PROFILE")
	}
	if profile == "" {
		return Rendered{}, false
	}
	content := profile
	if region := firstNonEmpty(ctx.Env("AWS_REGION"), ctx.Env("AWS_DEFAULT_REGION")); region != "" {
		content += " " + region
	}

	state := "DEFAULT"
	if strings.Contains(strings.ToLower(profile), "prod") {
		state = "PROD"
	}
	return Rendered{
		Content: content,
		State:   state,
		Icon:    decodeEscapes(cfg.Param("aws", state, "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

// renderAWSEBEnv shows the Elastic Beanstalk environment for this tree.
func renderAWSEBEnv(cfg *Config, ctx *Context) (Rendered, bool) {
	path, found := ctx.FindUp(filepath.Join(".elasticbeanstalk", "config.yml"))
	if !found {
		return Rendered{}, false
	}
	env := yamlNested(readFile(path), "environment")
	if env == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: env,
		Icon:    decodeEscapes(cfg.Param("aws_eb_env", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

// yamlNested finds a key at any depth, for documents where the value is
// not top level. First match wins.
func yamlNested(doc, key string) string {
	for line := range strings.SplitSeq(doc, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		if v := strings.Trim(strings.TrimSpace(value), `"'`); v != "" {
			return v
		}
	}
	return ""
}

// renderAzure shows the active subscription name.
func renderAzure(cfg *Config, ctx *Context) (Rendered, bool) {
	if name := ctx.Env("AZURE_SUBSCRIPTION_NAME"); name != "" {
		return Rendered{
			Content: name,
			Icon:    decodeEscapes(cfg.Param("azure", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
		}, true
	}
	// azureProfile.json marks one subscription "isDefault": true. Finding
	// it properly needs a JSON parse, which is not worth a prompt-path
	// dependency for a segment that the environment usually answers.
	return Rendered{}, false
}

// renderGcloud shows the active gcloud configuration.
func renderGcloud(cfg *Config, ctx *Context) (Rendered, bool) {
	config := ctx.Env("CLOUDSDK_ACTIVE_CONFIG_NAME")
	if config == "" {
		home := ctx.Env("CLOUDSDK_CONFIG")
		if home == "" {
			home = filepath.Join(ctx.Home, ".config", "gcloud")
		}
		config = firstLine(filepath.Join(home, "active_config"))
	}
	if config == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: config,
		Icon:    decodeEscapes(cfg.Param("gcloud", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

// renderGoogleAppCred warns that application-default credentials are in
// play — the environment variable that silently changes which identity
// half your tooling authenticates as.
func renderGoogleAppCred(cfg *Config, ctx *Context) (Rendered, bool) {
	path := ctx.Env("GOOGLE_APPLICATION_CREDENTIALS")
	if path == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: strings.TrimSuffix(filepath.Base(path), ".json"),
		Icon:    decodeEscapes(cfg.Param("google_app_cred", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

func renderToolbox(cfg *Config, ctx *Context) (Rendered, bool) {
	name := ctx.Env("TOOLBOX_NAME")
	if name == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: name,
		Icon:    decodeEscapes(cfg.Param("toolbox", "", "VISUAL_IDENTIFIER_EXPANSION", "⬢")),
	}, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
