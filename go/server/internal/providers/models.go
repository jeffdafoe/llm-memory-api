package providers

import (
	"bytes"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
)

// Registry types. Field order is the key order of Node's objects, so the
// admin UI's registry JSON is the same text.

// Pricing is dollars per million tokens; Request is dollars per thousand
// requests. A nil rate is not listed.
type Pricing struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	Request    *float64 `json:"request,omitempty"`
}

// rate is `rate || 0`.
func (Pricing) rate(v *float64) float64 {
	if v == nil || *v != *v {
		return 0
	}
	return *v
}

func (Pricing) truthy(v *float64) bool {
	return v != nil && *v != 0 && *v == *v
}

// LongContext is the gpt-6.x surcharge: above Threshold prompt tokens the
// whole request is billed at the multiplied rates.
type LongContext struct {
	Threshold float64 `json:"threshold"`
	Input     float64 `json:"input"`
	CacheRead float64 `json:"cache_read"`
	Output    float64 `json:"output"`
}

// DisabledWhen tells the admin UI when a control does not apply.
type DisabledWhen struct {
	Field     string `json:"field"`
	Condition string `json:"condition"`
	Value     string `json:"value"`
	Message   string `json:"message"`
}

// Capability is one configuration control.
type Capability struct {
	Name         string        `json:"-"`
	Type         string        `json:"type"`
	Label        string        `json:"label"`
	Description  string        `json:"description"`
	Default      any           `json:"default"`
	Min          *float64      `json:"min,omitempty"`
	Max          *float64      `json:"max,omitempty"`
	Step         *float64      `json:"step,omitempty"`
	Options      []string      `json:"options,omitempty"`
	DisabledWhen *DisabledWhen `json:"disabledWhen,omitempty"`
}

// Capabilities are a model's controls in order.
type Capabilities []Capability

// clone is a deep copy, so a caller cannot change the shared registry.
func (c Capabilities) clone() Capabilities {
	out := make(Capabilities, len(c))
	for i, cap := range c {
		cap.Min = copyFloat(cap.Min)
		cap.Max = copyFloat(cap.Max)
		cap.Step = copyFloat(cap.Step)
		cap.Options = append([]string(nil), cap.Options...)
		if cap.DisabledWhen != nil {
			d := *cap.DisabledWhen
			cap.DisabledWhen = &d
		}
		out[i] = cap
	}
	return out
}

func copyFloat(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

// clone is a deep copy.
func (p *Pricing) clone() *Pricing {
	if p == nil {
		return nil
	}
	return &Pricing{Input: copyFloat(p.Input), Output: copyFloat(p.Output), CacheWrite: copyFloat(p.CacheWrite), CacheRead: copyFloat(p.CacheRead), Request: copyFloat(p.Request)}
}

func (c Capabilities) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, cap := range c {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := jsonutil.Marshal(cap.Name)
		v, err := jsonutil.Marshal(cap)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Model is one registry entry.
type Model struct {
	ID            string       `json:"-"`
	Label         string       `json:"label"`
	APIID         string       `json:"apiId,omitempty"`
	ConfigVersion int          `json:"configVersion"`
	Deprecated    string       `json:"deprecated,omitempty"`
	Pricing       *Pricing     `json:"pricing"`
	LongContext   *LongContext `json:"longContext,omitempty"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Models are a provider's models in order.
type Models []*Model

// Get returns the model with this id, or nil.
func (m Models) Get(id string) *Model {
	for _, x := range m {
		if x.ID == id {
			return x
		}
	}
	return nil
}

func (m Models) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, x := range m {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := jsonutil.Marshal(x.ID)
		v, err := jsonutil.Marshal(x)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func num(f float64) *float64 {
	return &f
}

func price(input, output float64) *Pricing {
	return &Pricing{Input: num(input), Output: num(output)}
}

func numberCap(name, label, description string, def, min, max float64) Capability {
	return Capability{Name: name, Type: "number", Label: label, Description: description, Default: def, Min: num(min), Max: num(max)}
}

func stepCap(name, label, description string, def, min, max, step float64) Capability {
	c := numberCap(name, label, description, def, min, max)
	c.Step = num(step)
	return c
}

func selectCap(name, label, description string, def string, options ...string) Capability {
	return Capability{Name: name, Type: "select", Label: label, Description: description, Default: def, Options: options}
}

func boolCap(name, label, description string, def bool) Capability {
	return Capability{Name: name, Type: "boolean", Label: label, Description: description, Default: def}
}
