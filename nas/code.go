// Package nas talks to a Datame NAS the same way the phone app does: with a
// device token, over HTTPS, recognizing the NAS by the hash of its public key.
// See node/devices.go in the Datame repository.
package nas

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// CodePrefix starts every pairing code the NAS web makes in «Conexiones → Móviles».
const CodePrefix = "datame-movil:"

// Code is what a pairing code holds.
type Code struct {
	Version   int      `json:"v"`
	Addresses []string `json:"a"`
	Pin       string   `json:"p"` // SHA-256 of the SPKI of the NAS certificate, hex
	Token     string   `json:"t"` // dm1_<id>_<secret>
	Name      string   `json:"n"`
	Volume    string   `json:"vol,omitempty"`
	Folder    string   `json:"dir,omitempty"`
}

// ParseCode reads a code as pasted by a person. The errors are for people.
func ParseCode(text string) (Code, error) {
	text = strings.Join(strings.Fields(text), "") // copies from chats often break lines
	encoded, ok := strings.CutPrefix(text, CodePrefix)
	if !ok {
		return Code{}, errors.New("esto no es un código de Datame: créalo en la web del NAS, en Conexiones → Móviles → Añadir")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil {
		return Code{}, errors.New("el código está incompleto: vuelve a copiarlo entero")
	}
	var code Code
	if err := json.Unmarshal(data, &code); err != nil {
		return Code{}, errors.New("el código está incompleto: vuelve a copiarlo entero")
	}
	if code.Version != 1 {
		return Code{}, errors.New("este código es de otra versión de Datame: actualiza este programa")
	}
	code.Pin = strings.ToLower(code.Pin)
	if len(code.Addresses) == 0 || len(code.Pin) != 64 || !strings.HasPrefix(code.Token, "dm1_") {
		return Code{}, errors.New("el código está incompleto: vuelve a copiarlo entero")
	}
	for _, address := range code.Addresses {
		if !strings.HasPrefix(address, "https://") {
			return Code{}, errors.New("el código tiene una dirección que no es https://")
		}
	}
	return code, nil
}

// Encode makes the text of a code. The NAS does this; tests and tools use it too.
func (c Code) Encode() string {
	data, _ := json.Marshal(c)
	return CodePrefix + base64.RawURLEncoding.EncodeToString(data)
}
