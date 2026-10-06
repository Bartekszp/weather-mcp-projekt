package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cyberark/conjur-api-go/conjurapi"
	"github.com/cyberark/conjur-api-go/conjurapi/authn"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Dedykowany klient HTTP zapobiegający blokowaniu serwera
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

var iconMap = map[int]string{
	0: "tornado", 1: "tropicalstorm", 2: "hurricane", 3: "tstorms", 4: "tstorms",
	5: "rain", 6: "sleet", 7: "sleet", 8: "flurries", 9: "chancerain",
	10: "sleet", 11: "rain", 12: "rain", 13: "flurries", 14: "snow",
	15: "snow", 16: "snow", 17: "sleet", 18: "sleet", 19: "hazy",
	20: "fog", 21: "hazy", 22: "hazy", 23: "clear", 24: "clear",
	25: "clear", 26: "cloudy", 27: "nt_mostlycloudy", 28: "mostlycloudy",
	29: "nt_partlycloudy", 30: "partlycloudy", 31: "nt_clear", 32: "sunny",
	33: "nt_clear", 34: "mostlysunny", 35: "sleet", 36: "sunny",
	37: "tstorms", 38: "tstorms", 39: "chancerain", 40: "rain",
	41: "chancesnow", 42: "snow", 43: "snow", 44: "unknown",
	45: "nt_chancerain", 46: "nt_chancesnow", 47: "nt_tstorms",
}

func getIconName(code int) string {
	if name, ok := iconMap[code]; ok {
		return name
	}
	return "unknown"
}

func fetchSecretFromConjur(variableID string) (string, error) {
	config, err := conjurapi.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("błąd odczytu konfiguracji środowiskowej: %w", err)
	}

	login := os.Getenv("CONJUR_AUTHN_LOGIN")
	apiKey := os.Getenv("CONJUR_AUTHN_API_KEY")
	if login == "" || apiKey == "" {
		return "", fmt.Errorf("brak wymaganych zmiennych CONJUR_AUTHN_LOGIN lub CONJUR_AUTHN_API_KEY")
	}

	client, err := conjurapi.NewClientFromKey(config, authn.LoginPair{
		Login:  login,
		APIKey: apiKey,
	})
	if err != nil {
		return "", fmt.Errorf("błąd inicjalizacji klienta Conjur: %w", err)
	}

	secretBytes, err := client.RetrieveSecret(variableID)
	if err != nil {
		return "", fmt.Errorf("błąd pobierania sekretu: %w", err)
	}

	return string(secretBytes), nil
}

func main() {
	weatherApiKey, err := fetchSecretFromConjur("secrets/weather_api_key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Błąd Conjur: %v\n", err)
		os.Exit(1)
	}

	s := server.NewMCPServer("weather-service", "1.1.0")

	// 1. Narzędzie do geokodowania miast
	geoTool := mcp.NewTool("resolve_city_coords",
		mcp.WithDescription("Zwraca współrzędne geograficzne (lat, lng) dla podanej nazwy miasta. ZAWSZE używaj tego przed pobraniem pogody, gdy użytkownik podaje miasto zamiast współrzędnych."),
		mcp.WithString("city_name", mcp.Required(), mcp.Description("Nazwa miasta (np. 'Łódź', 'Warszawa')")),
	)

	s.AddTool(geoTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, ok := req.Params.Arguments.(map[string]any)
		if !ok {
			return mcp.NewToolResultError("Nieprawidłowy format argumentów"), nil
		}

		cityName, ok := args["city_name"].(string)
		if !ok || strings.TrimSpace(cityName) == "" {
			return mcp.NewToolResultError("Wymagany parametr 'city_name'"), nil
		}

		geoURL := fmt.Sprintf("https://geocoding-api.open-meteo.com/v1/search?name=%s&count=1&language=pl&format=json", url.QueryEscape(cityName))
		resp, err := httpClient.Get(geoURL)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Błąd geokodowania: %v", err)), nil
		}
		defer resp.Body.Close()

		var geoData struct {
			Results []struct {
				Name      string  `json:"name"`
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
				Country   string  `json:"country"`
			} `json:"results"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&geoData); err != nil {
			return mcp.NewToolResultError("Błąd parsowania odpowiedzi geokodowania"), nil
		}

		if len(geoData.Results) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("Nie znaleziono lokalizacji: '%s'", cityName)), nil
		}

		res := geoData.Results[0]
		out := fmt.Sprintf("Znaleziono: %s, %s\nLat: %f\nLng: %f", res.Name, res.Country, res.Latitude, res.Longitude)
		return mcp.NewToolResultText(out), nil
	})

	// 2. Aktualna pogoda (zoptymalizowana)
	weatherTool := mcp.NewTool("get_weather_by_coords",
		mcp.WithDescription("Pobiera aktualną pogodę na podstawie DOKŁADNYCH współrzędnych geograficznych. Zwraca dane metryczne (°C, km/h) oraz ikonę pogody. WAŻNE: ZAWSZE umieszczaj zwrócony znacznik Markdown grafiki (![...](...)) w swojej ostatecznej odpowiedzi dla użytkownika."),
		mcp.WithNumber("lat", mcp.Required(), mcp.Description("Szerokość geograficzna (np. 51.7592)")),
		mcp.WithNumber("lng", mcp.Required(), mcp.Description("Długość geograficzna (np. 19.4560)")),
	)

	s.AddTool(weatherTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, ok := req.Params.Arguments.(map[string]any)
		if !ok {
			return mcp.NewToolResultError("Nieprawidłowy format argumentów"), nil
		}

		lat, ok1 := args["lat"].(float64)
		lng, ok2 := args["lng"].(float64)
		if !ok1 || !ok2 {
			return mcp.NewToolResultError("Wymagane parametry liczbowe lat i lng"), nil
		}

		apiURL := fmt.Sprintf("https://api.weather.com/v1/geocode/%f/%f/observations/current.json?apiKey=%s&units=m&language=pl-PL", lat, lng, weatherApiKey)
		resp, err := httpClient.Get(apiURL)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Nie udało się pobrać aktualnej pogody: %v", err)), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("API pogodowe zwróciło błąd (HTTP %d)", resp.StatusCode)), nil
		}

		var weatherData struct {
			Observation struct {
				Phrase32Char string `json:"phrase_32char"`
				IconCode     int    `json:"icon_code"`
				Metric       struct {
					Temp      int `json:"temp"`
					FeelsLike int `json:"feels_like"`
					Rh        int `json:"rh"`
					Wspd      int `json:"wspd"`
				} `json:"metric"`
			} `json:"observation"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&weatherData); err != nil {
			return mcp.NewToolResultError("Błąd dekodowania odpowiedzi pogody"), nil
		}

		obs := weatherData.Observation
		iconURL := fmt.Sprintf("https://raw.githubusercontent.com/manifestinteractive/weather-underground-icons/master/dist/icons/black/png/128x128/%s.png", getIconName(obs.IconCode))

		summary := fmt.Sprintf(
			"![%s](%s)\n\nWarunki: %s\nTemperatura: %d°C (odczuwalna: %d°C)\nWilgotność: %d%%\nWiatr: %d km/h\n\n[INSTRUKCJA DLA ASYSTENTA AI: Aby użytkownik widział ikonę pogody, ZAWSZE skopiuj i umieść powyższy znacznik grafiki `![%s](%s)` w swojej ostatecznej odpowiedzi dla użytkownika!]",
			obs.Phrase32Char, iconURL, obs.Phrase32Char, obs.Metric.Temp, obs.Metric.FeelsLike, obs.Metric.Rh, obs.Metric.Wspd, obs.Phrase32Char, iconURL,
		)

		return mcp.NewToolResultText(summary), nil
	})

	// 3. Prognoza pogody (zoptymalizowana)
	forecastTool := mcp.NewTool("get_forecast_by_coords",
		mcp.WithDescription("Pobiera prognozę pogody (na kolejne dni) na podstawie dokładnych współrzędnych geograficznych. WAŻNE: ZAWSZE umieszczaj zwrócone znaczniki Markdown grafik (![...](...)) w swojej ostatecznej odpowiedzi dla użytkownika przy każdym dniu."),
		mcp.WithNumber("lat", mcp.Required(), mcp.Description("Szerokość geograficzna (np. 51.7592)")),
		mcp.WithNumber("lng", mcp.Required(), mcp.Description("Długość geograficzna (np. 19.4560)")),
	)

	s.AddTool(forecastTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, ok := req.Params.Arguments.(map[string]any)
		if !ok {
			return mcp.NewToolResultError("Nieprawidłowy format argumentów"), nil
		}

		lat, ok1 := args["lat"].(float64)
		lng, ok2 := args["lng"].(float64)
		if !ok1 || !ok2 {
			return mcp.NewToolResultError("Wymagane parametry liczbowe lat i lng"), nil
		}

		apiURL := fmt.Sprintf("https://api.weather.com/v1/geocode/%f/%f/forecast/daily/5day.json?apiKey=%s&units=m&language=pl-PL", lat, lng, weatherApiKey)
		resp, err := httpClient.Get(apiURL)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Nie udało się pobrać prognozy pogody: %v", err)), nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("API prognozy zwróciło błąd (HTTP %d)", resp.StatusCode)), nil
		}

		type Daypart struct {
			DaypartName     string `json:"daypart_name"`
			LongDaypartName string `json:"long_daypart_name"`
			Temp            int    `json:"temp"`
			IconCode        int    `json:"icon_code"`
			Phrase32Char    string `json:"phrase_32char"`
			Narrative       string `json:"narrative"`
			Pop             int    `json:"pop"`
			Wspd            int    `json:"wspd"`
		}

		var forecastData struct {
			Forecasts []struct {
				Dow   string   `json:"dow"`
				Day   *Daypart `json:"day"`
				Night *Daypart `json:"night"`
			} `json:"forecasts"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&forecastData); err != nil {
			return mcp.NewToolResultError("Błąd dekodowania prognozy pogody"), nil
		}

		var sb strings.Builder
		sb.WriteString("### Prognoza pogody na kolejne dni:\n\n")

		count := 0
		for _, dayFc := range forecastData.Forecasts {
			parts := []*Daypart{}
			if dayFc.Day != nil {
				parts = append(parts, dayFc.Day)
			}
			if dayFc.Night != nil {
				parts = append(parts, dayFc.Night)
			}

			for _, dp := range parts {
				if count >= 4 {
					break
				}
				count++

				iconURL := fmt.Sprintf("https://raw.githubusercontent.com/manifestinteractive/weather-underground-icons/master/dist/icons/black/png/128x128/%s.png", getIconName(dp.IconCode))
				label := dp.LongDaypartName
				if label == "" {
					label = dp.DaypartName
				}
				if label == "" {
					label = dayFc.Dow
				}

				sb.WriteString(fmt.Sprintf("#### %s\n", label))
				sb.WriteString(fmt.Sprintf("![%s](%s)\n\n", dp.Phrase32Char, iconURL))
				sb.WriteString(fmt.Sprintf("* **Warunki:** %s\n* **Temperatura:** %d°C\n* **Szansa opadów:** %d%%\n* **Wiatr:** %d km/h\n* **Opis:** %s\n\n",
					dp.Phrase32Char, dp.Temp, dp.Pop, dp.Wspd, dp.Narrative))
			}
			if count >= 4 {
				break
			}
		}

		sb.WriteString("[INSTRUKCJA DLA ASYSTENTA AI: Zachowaj i umieść wszystkie powyższe znaczniki grafik Markdown `![...](...)` w swojej ostatecznej odpowiedzi dla każdego dnia/nocy!]\n")

		return mcp.NewToolResultText(sb.String()), nil
	})

	if err := server.ServeStdio(s); err != nil {
		panic(err)
	}
}
