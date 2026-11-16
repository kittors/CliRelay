package translatortest

import (
	"fmt"
	"math/rand"
	"strings"
)

// GeminiRequest returns a random Gemini generateContent request covering what
// the translators read: a system instruction under either key, contents of
// each role (missing and unknown ones too) whose parts hold text, thoughts,
// thought signatures of each family, function calls and responses (with and
// without ids, args or images), inline and file data, unknown and scalar
// parts, the odd non-array contents or parts, tools declared under both
// spellings with parameters or parametersJsonSchema and upper-case schema
// types, built-in tools, toolConfig, generationConfig with thinking and
// response schemas, and safety settings, sometimes indented.
func GeminiRequest(r *rand.Rand) []byte {
	return gen{r}.indent(`{"model":"gemini-2.5-pro",` + gen{r}.geminiBody() + `}`)
}

// GeminiCLIRequest wraps a random Gemini request the way Gemini CLI sends it.
func GeminiCLIRequest(r *rand.Rand) []byte {
	g := gen{r}
	return g.indent(`{"model":"gemini-2.5-pro","project":"p-1","request":{` + g.geminiBody() + `}}`)
}

func (g gen) geminiBody() string {
	var fields []string
	switch g.r.Intn(5) {
	case 0:
		fields = append(fields, `"systemInstruction":{"parts":[{"text":`+g.text()+`}]}`)
	case 1:
		fields = append(fields, `"system_instruction":{"role":"user","parts":[{"text":"sys a"},{"text":`+g.text()+`},{"inlineData":{"mimeType":"image/png","data":"QUJD"}}]}`)
	case 2:
		fields = append(fields, `"systemInstruction":`+g.pick(`{"parts":"not parts"}`, `{"text":"no parts"}`, `"string"`))
	}
	switch g.r.Intn(12) {
	case 0:
	case 1:
		fields = append(fields, `"contents":`+g.pick(`{"role":"user","parts":[{"text":"object"}]}`, `"scalar"`, `[]`))
	default:
		var contents []string
		for n := g.r.Intn(12); n > 0; n-- {
			contents = append(contents, g.geminiContents()...)
		}
		fields = append(fields, `"contents":[`+strings.Join(contents, ",")+`]`)
	}
	if g.chance(3) {
		fields = append(fields, `"tools":[`+g.geminiTools()+`]`)
	}
	if g.chance(4) {
		fields = append(fields, `"toolConfig":{"functionCallingConfig":{"mode":`+g.pick(`"NONE"`, `"AUTO"`, `"ANY"`, `"VALIDATED"`)+`}}`)
	}
	if g.chance(2) {
		fields = append(fields, `"generationConfig":{`+g.geminiGenerationConfig()+`}`)
	}
	if g.chance(5) {
		fields = append(fields, `"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]`)
	}
	g.r.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
	return strings.Join(fields, ",")
}

func (g gen) geminiGenerationConfig() string {
	var fields []string
	if g.chance(2) {
		fields = append(fields, `"temperature":0.3,"topP":0.9,"topK":40,"maxOutputTokens":`+g.pick(`2048`, `"lots"`))
	}
	if g.chance(3) {
		fields = append(fields, `"stopSequences":`+g.pick(`["END"]`, `"END"`, `[]`), `"candidateCount":`+g.pick(`1`, `2`))
	}
	if g.chance(2) {
		fields = append(fields, `"thinkingConfig":`+g.pick(
			`{"thinkingLevel":"high","includeThoughts":true}`,
			`{"thinking_level":"LOW"}`,
			`{"thinkingBudget":2048}`,
			`{"thinking_budget":0}`,
			`{"thinkingBudget":-1,"includeThoughts":false}`,
			`{"includeThoughts":true}`,
			`"off"`,
		))
	}
	if g.chance(4) {
		fields = append(fields, `"responseMimeType":"application/json"`, g.pick(`"responseSchema":{"type":"OBJECT","properties":{"a":{"type":"STRING"}}}`, `"responseJsonSchema":{"type":"object"}`))
	}
	return strings.Join(fields, ",")
}

func (g gen) geminiPart(model bool) string {
	switch g.r.Intn(12) {
	case 0, 1, 2:
		return `{"text":` + g.text() + `}`
	case 3:
		if model {
			return `{"text":` + g.text() + `,"thought":true` + g.pick(``, `,"thoughtSignature":`+g.signature()) + `}`
		}
		return `{"text":"user text"}`
	case 4, 5:
		args := g.pick(`,"args":{"cmd":["ls"]}`, `,"args":{}`, ``, `,"args":"{\"a\":1}"`, `,"args":{"nested":{"a":"<b>&"}}`)
		id := g.pick(``, `,"id":`+g.id("fc_"))
		return `{"functionCall":{"name":` + g.pick(`"shell"`, `"read"`, `""`) + args + id + `}` + g.pick(``, `,"thoughtSignature":`+g.signature()) + `}`
	case 6, 7:
		return `{"functionResponse":{"name":` + g.pick(`"shell"`, `"read"`) + g.pick(``, `,"id":`+g.id("fc_")) + `,"response":` + g.pick(
			`{"result":`+g.text()+`}`,
			`{"output":"done"}`,
			`{"result":{"ok":true}}`,
			`{}`,
			`"bare string"`,
		) + g.pick(``, `,"parts":[{"inlineData":{"mimeType":"image/png","data":"QUJD"}}]`) + `}}`
	case 8:
		return g.pick(
			`{"inlineData":{"mimeType":"image/png","data":"`+g.base64Data()+`"}}`,
			`{"inlineData":{"mime_type":"image/jpeg","data":"QUJD"}}`,
			`{"inlineData":{"data":"QUJD"}}`,
		)
	case 9:
		return `{"fileData":{"mimeType":"application/pdf","fileUri":"gs://b/f.pdf"}}`
	case 10:
		return `{"thoughtSignature":` + g.signature() + `}`
	default:
		return g.pick(`{}`, `"scalar part"`, `{"executableCode":{"code":"print(1)"}}`)
	}
}

func (g gen) geminiContent(role string) string {
	model := role == `"model"`
	head := `{`
	if role != "" {
		head += `"role":` + role + `,`
	}
	switch g.r.Intn(12) {
	case 0:
		return head + `"parts":` + g.pick(`{"text":"object parts"}`, `"string parts"`, `[]`) + `}`
	case 1:
		return strings.TrimSuffix(head, ",") + `}`
	default:
		var parts []string
		for n := 1 + g.r.Intn(4); n > 0; n-- {
			parts = append(parts, g.geminiPart(model))
		}
		return head + `"parts":[` + strings.Join(parts, ",") + `]}`
	}
}

func (g gen) geminiContents() []string {
	switch g.r.Intn(10) {
	case 0, 1, 2:
		return []string{g.geminiContent(`"user"`)}
	case 3, 4, 5:
		return []string{g.geminiContent(`"model"`)}
	case 6, 7:
		// A tool round: calls from the model, then the responses.
		var calls, responses []string
		for n := 1 + g.r.Intn(3); n > 0; n-- {
			name := g.pick(`"shell"`, `"read"`)
			calls = append(calls, `{"functionCall":{"name":`+name+`,"args":{"cmd":["ls"]}}`+g.pick(``, `,"thoughtSignature":`+g.signature())+`}`)
			responses = append(responses, `{"functionResponse":{"name":`+name+`,"response":{"result":`+g.text()+`}}}`)
		}
		role := g.pick(`"user"`, `"function"`, `"tool"`)
		return []string{
			`{"role":"model","parts":[` + strings.Join(calls, ",") + `]}`,
			`{"role":` + role + `,"parts":[` + strings.Join(responses[:g.r.Intn(len(responses)+1)], ",") + `]}`,
		}
	default:
		return []string{g.geminiContent(g.pick(``, `"function"`, `"system"`, `"assistant"`, `7`)), g.pick(`"scalar content"`, `{}`)}[:1+g.r.Intn(2)]
	}
}

func (g gen) geminiDeclaration() string {
	params := g.pick(`,"parameters":`+g.schema(), `,"parametersJsonSchema":`+g.schema(), ``, `,"parameters":`+g.schema()+`,"parametersJsonSchema":{"type":"object"}`)
	return `{"name":` + g.pick(`"shell"`, `"read"`, `"a_very_long_tool_name_that_goes_past_the_sixty_four_character_limit_x"`) + `,"description":"tool"` + params + `}`
}

func (g gen) geminiTools() string {
	var tools []string
	for n := g.r.Intn(4); n > 0; n-- {
		var decls []string
		for m := g.r.Intn(3); m > 0; m-- {
			decls = append(decls, g.geminiDeclaration())
		}
		list := `[` + strings.Join(decls, ",") + `]`
		tools = append(tools, g.pick(
			`{"functionDeclarations":`+list+`}`,
			`{"function_declarations":`+list+`}`,
			`{"functionDeclarations":`+list+`,"function_declarations":[{"name":"old"}]}`,
			`{"googleSearch":{}}`,
			`{"codeExecution":{}}`,
			`{"functionDeclarations":"not a list"}`,
			`"scalar tool"`,
		))
	}
	return strings.Join(tools, ",")
}

// GeminiSession is a Gemini request with turns function calls, each answered
// by a function response, and a screenshot every fifth turn.
func GeminiSession(turns int) []byte {
	return []byte(`{"model":"gemini-2.5-pro",` + geminiSessionBody(turns) + `}`)
}

// GeminiCLISession is GeminiSession wrapped the way Gemini CLI sends it.
func GeminiCLISession(turns int) []byte {
	return []byte(`{"model":"gemini-2.5-pro","project":"p-1","request":{` + geminiSessionBody(turns) + `}}`)
}

func geminiSessionBody(turns int) string {
	contents := []string{`{"role":"user","parts":[{"text":"fix the build"}]}`}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("step %d", i)
		contents = append(contents,
			`{"role":"model","parts":[{"text":"`+id+`","thought":true,"thoughtSignature":"`+sessionSignature+`"},{"functionCall":{"name":"shell","args":{"cmd":["ls","-la"]}},"thoughtSignature":"`+sessionSignature+`"}]}`,
			`{"role":"user","parts":[{"functionResponse":{"name":"shell","response":{"result":"`+strings.Repeat("line of tool output ", 100)+`"}}}]}`,
		)
		if i%5 == 4 {
			contents = append(contents, `{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"`+strings.Repeat("iVBORw0K", 2000)+`"}}]}`)
		}
	}
	return `"systemInstruction":{"parts":[{"text":"be brief"}]},"contents":[` + strings.Join(contents, ",") + `],"tools":[{"functionDeclarations":[{"name":"shell","description":"run","parameters":{"type":"OBJECT","properties":{"cmd":{"type":"ARRAY","items":{"type":"STRING"}}}}}]}]`
}
