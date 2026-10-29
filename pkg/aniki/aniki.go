// Package aniki is the gym's mentor: he reads a workload and says, kindly
// and once, what will hurt it.
package aniki

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Finding is one thing Aniki has to say about an object.
type Finding struct {
	Object  string `json:"object"`
	Path    string `json:"path"`
	Advice  string `json:"advice"`
	Serious bool   `json:"serious"`
}

func (f Finding) String() string {
	mark := "·"
	if f.Serious {
		mark = "!"
	}
	return fmt.Sprintf("%s %s %s: %s", mark, f.Object, f.Path, f.Advice)
}

// podSpec is where an object keeps its pod: a Pod's spec, a workload's
// template, a CronJob's job template.
func podSpec(u *unstructured.Unstructured) (map[string]any, string, bool) {
	switch u.GetKind() {
	case "Pod":
		m, ok, _ := unstructured.NestedMap(u.Object, "spec")
		return m, "spec", ok
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
		m, ok, _ := unstructured.NestedMap(u.Object, "spec", "template", "spec")
		return m, "spec.template.spec", ok
	case "CronJob":
		m, ok, _ := unstructured.NestedMap(u.Object, "spec", "jobTemplate", "spec", "template", "spec")
		return m, "spec.jobTemplate.spec.template.spec", ok
	}
	return nil, "", false
}

// Review is what Aniki says about one object.
func Review(u *unstructured.Unstructured) []Finding {
	name := u.GetKind() + " " + u.GetName()
	var out []Finding
	say := func(path, advice string, serious bool) {
		out = append(out, Finding{Object: name, Path: path, Advice: advice, Serious: serious})
	}
	spec, base, ok := podSpec(u)
	if !ok {
		return nil
	}
	serves := u.GetKind() == "Deployment" || u.GetKind() == "StatefulSet"
	if serves {
		// Numbers come as int64 from the API, float64 from YAML.
		replicas := int64(1)
		switch v := u.Object["spec"].(map[string]any)["replicas"].(type) {
		case int64:
			replicas = v
		case float64:
			replicas = int64(v)
		}
		if replicas < 2 {
			say("spec.replicas", "a one-man gym: anything that serves needs two to keep standing while one rests", false)
		} else if _, spread := spec["topologySpreadConstraints"]; !spread && !hasAntiAffinity(spec) {
			say(base, "all your boys on one bench: spread the replicas over nodes (topologySpreadConstraints)", false)
		}
	}
	if b, _, _ := unstructured.NestedBool(spec, "hostNetwork"); b {
		say(base+".hostNetwork", "you share the host's network: only for the cluster's own agents", true)
	}
	vols, _, _ := unstructured.NestedSlice(spec, "volumes")
	for _, v := range vols {
		if m, _ := v.(map[string]any); m != nil {
			if _, hp := m["hostPath"]; hp {
				say(base+".volumes."+fmt.Sprint(m["name"]), "a host path ties you to one node and opens it to you: use a volume", true)
			}
		}
	}
	for _, field := range []string{"initContainers", "containers"} {
		ctrs, _, _ := unstructured.NestedSlice(spec, field)
		for _, c := range ctrs {
			m, _ := c.(map[string]any)
			if m == nil {
				continue
			}
			p := fmt.Sprintf("%s.%s.%v", base, field, m["name"])
			image, _ := m["image"].(string)
			switch {
			case !strings.Contains(image, "@") && !strings.Contains(lastPart(image), ":"):
				say(p+".image", "no tag is :latest in disguise: pin the ♂right version♂, a tag or a digest", true)
			case strings.HasSuffix(image, ":latest"):
				say(p+".image", ":latest changes under you: pin the ♂right version♂, a tag or a digest", true)
			}
			res, _ := m["resources"].(map[string]any)
			if _, ok := res["requests"]; !ok {
				say(p+".resources", "lifting without a plan: set requests, so the scheduler knows your weight", true)
			}
			if field == "containers" && u.GetKind() != "Job" && u.GetKind() != "CronJob" {
				if _, ok := m["readinessProbe"]; !ok {
					say(p+".readinessProbe", "no spotter for traffic: a readiness probe keeps requests off you while you warm up", false)
				}
			}
			sc, _ := m["securityContext"].(map[string]any)
			if b, _ := sc["privileged"].(bool); b {
				say(p+".securityContext.privileged", "privileged is the whole host in your hands: leather says no", true)
			}
			if b, ok := sc["allowPrivilegeEscalation"].(bool); !ok || b {
				say(p+".securityContext.allowPrivilegeEscalation", "set it false: nobody climbs over the ropes", false)
			}
		}
	}
	return out
}

func lastPart(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

func hasAntiAffinity(spec map[string]any) bool {
	_, ok, _ := unstructured.NestedMap(spec, "affinity", "podAntiAffinity")
	return ok
}

// Warnings are the serious findings as admission warnings.
func Warnings(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		if f.Serious {
			out = append(out, "♂ Aniki: "+f.Path+": "+f.Advice)
		}
	}
	return out
}
