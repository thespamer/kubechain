// pkg/ai/validator.go
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DiffPayload é o payload enviado ao AI Validator.
type DiffPayload struct {
	ResourceKind         string `json:"resourceKind"`
	ResourceName         string `json:"resourceName"`
	Namespace            string `json:"namespace"`
	CurrentState         []byte `json:"currentState"`
	ProposedManifest     []byte `json:"proposedManifest"`
	ChangelogDescription string `json:"changelogDescription"`
	PRReference          string `json:"prReference,omitempty"`
	RequestedBy          string `json:"requestedBy"`
}

// ValidationResult é o resultado retornado pelo AI Validator.
type ValidationResult struct {
	RiskScore float64  `json:"riskScore"`     // 0.0 = seguro, 1.0 = crítico
	Summary   string   `json:"summary"`       // explicação em linguagem natural
	Findings  []string `json:"findings"`      // lista de problemas encontrados
	Approved  bool     `json:"approved"`
}

// ValidatorClient chama o AI Validator sidecar via HTTP.
// O sidecar roda como um container no mesmo pod do operator,
// com acesso a um LLM local (Ollama/OpenAI compatível).
type ValidatorClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewValidatorClient(baseURL string) *ValidatorClient {
	return &ValidatorClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second, // LLM pode demorar
		},
	}
}

// Validate analisa o diff usando o LLM e retorna um score de risco.
//
// O prompt enviado ao LLM é estruturado para responder:
//   1. O manifest proposto é semanticamente consistente com o changelog?
//   2. Há operações suspeitas (wildcard verbs, exposição de secrets, etc.)?
//   3. A mudança respeita as políticas de segurança da organização?
//
// O LLM responde em JSON estruturado com score + reasoning.
func (c *ValidatorClient) Validate(ctx context.Context, payload *DiffPayload) (*ValidationResult, error) {
	// Monta o prompt estruturado para o LLM
	prompt := buildValidationPrompt(payload)

	reqBody := map[string]interface{}{
		"prompt":      prompt,
		"max_tokens":  1024,
		"temperature": 0.1, // baixa temperatura para respostas mais determinísticas
		"format":      "json",
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("erro serializando request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/api/validate", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AI validator indisponível: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AI validator retornou status %d", resp.StatusCode)
	}

	var result ValidationResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("resposta inválida do AI validator: %w", err)
	}

	return &result, nil
}

// buildValidationPrompt constrói o prompt para o LLM analisar o diff.
func buildValidationPrompt(p *DiffPayload) string {
	return fmt.Sprintf(`Você é um especialista em segurança Kubernetes analisando uma proposta de mudança de configuração.

## Contexto da Mudança
- Tipo de resource: %s
- Nome: %s
- Namespace: %s
- Solicitado por: %s
- PR de referência: %s

## Descrição da Mudança (changelog)
%s

## Estado Atual do Resource
%s

## Manifest Proposto
%s

## Sua Análise

Responda APENAS com JSON no formato abaixo. Sem texto adicional.

{
  "riskScore": <float entre 0.0 e 1.0>,
  "summary": "<resumo em 2-3 frases do que a mudança faz e por quê o score foi esse>",
  "findings": [
    "<finding 1: descreva problemas específicos encontrados>",
    "<finding 2: inconsistências entre changelog e manifest>",
    "<finding 3: problemas de segurança>"
  ],
  "approved": <true se riskScore < 0.85>
}

## Critérios de Score
- 0.0–0.3: mudança segura, consistente com changelog, sem problemas
- 0.3–0.6: mudança razoável, pequenas inconsistências ou riscos menores
- 0.6–0.85: atenção necessária — inconsistências ou riscos significativos
- 0.85–1.0: BLOQUEADO — changelog inconsistente com manifest, operações perigosas, ou escalada de privilégios

## Verificações Obrigatórias
1. O manifest proposto é consistente com a descrição do changelog?
2. Há wildcard verbs (verbs: ["*"]) em ClusterRoles?
3. Há exposição de Secrets via env ou volumeMounts inseguros?
4. Há mudanças em NetworkPolicy que abrem comunicação não justificada?
5. Há adição de capabilities perigosas (SYS_ADMIN, NET_ADMIN, etc.)?
6. O changelog menciona "ajuste menor" mas o manifest tem mudanças estruturais grandes?
`,
		p.ResourceKind, p.ResourceName, p.Namespace,
		p.RequestedBy, p.PRReference,
		p.ChangelogDescription,
		string(p.CurrentState),
		string(p.ProposedManifest),
	)
}
