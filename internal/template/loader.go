package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type ResourceTemplate struct {
	Resources       []RenderedResource
	RenderedObjects []json.RawMessage
	ReturnValues    map[string]string
}

type RenderedResource struct {
	APIVersion  string            `json:"apiVersion,omitempty"`
	Kind        string            `json:"kind,omitempty"`
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func LoadResourceTemplate(templatePath, valuesPath, id string) (ResourceTemplate, error) {
	templateData, err := os.ReadFile(templatePath)
	if err != nil {
		return ResourceTemplate{}, fmt.Errorf("read template file: %w", err)
	}

	valuesData, err := os.ReadFile(valuesPath)
	if err != nil {
		return ResourceTemplate{}, fmt.Errorf("read values file: %w", err)
	}

	return loadResourceTemplateFromData(templatePath, templateData, valuesData, id)
}

func LoadResourceTemplateFromValuesData(templatePath string, valuesData []byte, id string) (ResourceTemplate, error) {
	log.Printf("Loading values from provided bytes")
	log.Printf("Loading template from %s", templatePath)

	templateData, err := os.ReadFile(templatePath)
	if err != nil {
		return ResourceTemplate{}, fmt.Errorf("read template file: %w", err)
	}

	return loadResourceTemplateFromData(templatePath, templateData, valuesData, id)
}

func loadResourceTemplateFromData(templatePath string, templateData, valuesData []byte, id string) (ResourceTemplate, error) {

	values, err := chartutil.ReadValues(valuesData)
	if err != nil {
		return ResourceTemplate{}, fmt.Errorf("decode values file: %w", err)
	}

	chartName := "claim-" + id
	templateName := filepath.Base(templatePath)
	chartObj := &chart.Chart{
		Metadata: &chart.Metadata{
			APIVersion: "v2",
			Name:       chartName,
			Version:    "0.1.0",
		},
		Templates: []*chart.File{
			{Name: filepath.ToSlash(filepath.Join("templates", templateName)), Data: templateData},
		},
	}

	renderValues, err := chartutil.ToRenderValues(
		chartObj,
		values,
		chartutil.ReleaseOptions{
			Name:      chartName,
			Namespace: "default",
			Revision:  1,
			IsInstall: true,
		},
		chartutil.DefaultCapabilities,
	)
	if err != nil {
		return ResourceTemplate{}, fmt.Errorf("build helm render values: %w", err)
	}

	renderedMap, err := engine.Render(chartObj, renderValues)
	if err != nil {
		return ResourceTemplate{}, fmt.Errorf("render helm template: %w", err)
	}

	renderedText, err := pickRenderedTemplate(renderedMap, templateName)
	if err != nil {
		return ResourceTemplate{}, err
	}

	var rendered bytes.Buffer
	rendered.WriteString(renderedText)

	var result ResourceTemplate
	result.ReturnValues = map[string]string{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered.Bytes()), 4096)
	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return ResourceTemplate{}, fmt.Errorf("decode YAML document: %w", err)
		}
		if len(raw) == 0 {
			continue
		}

		if _, ok := raw["kind"].(string); !ok {
			continue
		}

		result.Resources = append(result.Resources, renderedResourceFromRaw(raw))
		bytesDoc, err := toJSON(raw)
		if err != nil {
			return ResourceTemplate{}, fmt.Errorf("marshal rendered resource: %w", err)
		}
		result.RenderedObjects = append(result.RenderedObjects, bytesDoc)
		aggregateReturnValues(result.ReturnValues, raw)
	}

	if len(result.RenderedObjects) == 0 {
		return ResourceTemplate{}, fmt.Errorf("template must render at least one resource")
	}

	return result, nil
}

func renderedResourceFromRaw(raw map[string]any) RenderedResource {
	resource := RenderedResource{
		Labels:      map[string]string{},
		Annotations: map[string]string{},
	}

	if apiVersion, ok := raw["apiVersion"].(string); ok {
		resource.APIVersion = apiVersion
	}
	if kind, ok := raw["kind"].(string); ok {
		resource.Kind = kind
	}

	metadata, _ := raw["metadata"].(map[string]any)
	if name, ok := metadata["name"].(string); ok {
		resource.Name = name
	}
	if namespace, ok := metadata["namespace"].(string); ok {
		resource.Namespace = namespace
	}

	if labels := toStringMap(metadata["labels"]); labels != nil {
		resource.Labels = labels
	}
	if annotations := toStringMap(metadata["annotations"]); annotations != nil {
		resource.Annotations = annotations
	}

	if len(resource.Labels) == 0 {
		resource.Labels = nil
	}
	if len(resource.Annotations) == 0 {
		resource.Annotations = nil
	}

	return resource
}

func aggregateReturnValues(target map[string]string, raw map[string]any) {
	metadata, _ := raw["metadata"].(map[string]any)
	if metadata == nil {
		return
	}

	labels := toStringMap(metadata["labels"])
	annotations := toStringMap(metadata["annotations"])

	for _, source := range []map[string]string{labels, annotations} {
		if source == nil {
			continue
		}

		rawDirective, ok := source["claim.controller/return"]
		if !ok || strings.TrimSpace(rawDirective) == "" {
			continue
		}

		for key, value := range parseKeyValuePairs(rawDirective) {
			target[key] = value
		}
	}
}

func parseKeyValuePairs(s string) map[string]string {
	out := map[string]string{}

	normalized := strings.NewReplacer("\n", ",", ";", ",").Replace(s)
	for _, token := range strings.Split(normalized, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		parts := strings.SplitN(token, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}

		out[key] = value
	}

	return out
}

func toStringMap(value any) map[string]string {
	rawMap, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	out := make(map[string]string, len(rawMap))
	for key, item := range rawMap {
		out[key] = fmt.Sprint(item)
	}
	return out
}

func pickRenderedTemplate(rendered map[string]string, templateName string) (string, error) {
	templateSuffix := "/templates/" + templateName
	for path, content := range rendered {
		if strings.HasSuffix(path, templateSuffix) {
			return content, nil
		}
	}

	if len(rendered) == 1 {
		for _, content := range rendered {
			return content, nil
		}
	}

	keys := make([]string, 0, len(rendered))
	for key := range rendered {
		keys = append(keys, key)
	}
	return "", fmt.Errorf("rendered template %q not found, available templates: %s", templateName, strings.Join(keys, ", "))
}

func toJSON(raw map[string]any) ([]byte, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
