package responses

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini-cli/gemini"
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/openai/responses"
)

func ConvertOpenAIResponsesRequestToGeminiCLI(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	var err error
	rawJSON, err = ConvertOpenAIResponsesRequestToGemini(modelName, rawJSON, stream)
	if err != nil {
		return inputRawJSON
	}
	return ConvertGeminiRequestToGeminiCLI(modelName, rawJSON, stream)
}
