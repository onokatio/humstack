package meta

type ResourceType string

type Meta struct {
	Name        string            `json:"name" yaml:"name"`
	Namespace   string            `json:"namespace" yaml:"namespace"`
	Annotations map[string]string `json:"annotations" yaml:"annotations"`
	Labels      map[string]string `json:"labels" yaml:"labels"`
}
