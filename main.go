package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

func main() {
	// Serve the HTML file for the root route
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	// Search endpoint to fetch results from Wikimedia API
	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if query == "" {
			log.Println("Query parameter is missing")
			http.Error(w, "Query parameter is required", http.StatusBadRequest)
			return
		}

		category := r.URL.Query().Get("category")
		if category == "" {
			log.Println("Category parameter is missing")
			http.Error(w, "Category parameter is required", http.StatusBadRequest)
			return
		}

		encodedQuery := url.QueryEscape(query)
		searchUrl := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=opensearch&format=json&search=%s", encodedQuery)
		log.Printf("Fetching data from Wikimedia API: %s\n", searchUrl)
		resp, err := http.Get(searchUrl)
		if err != nil {
			log.Printf("Failed to fetch data from Wikimedia API: %v\n", err)
			http.Error(w, "Failed to fetch data from Wikimedia API", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body: %v\n", err)
			http.Error(w, "Failed to read response body", http.StatusInternalServerError)
			return
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("Wikimedia API returned non-200 status: %d\nResponse body: %s\n", resp.StatusCode, string(body))
			http.Error(w, "Failed to fetch data from Wikimedia API", http.StatusInternalServerError)
			return
		}

		var data []interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			log.Printf("Failed to parse JSON response: %v\n", err)
			http.Error(w, "Failed to parse JSON response", http.StatusInternalServerError)
			return
		}

		results, ok := data[1].([]interface{})
		if !ok {
			log.Println("Unexpected response format from Wikimedia API")
			http.Error(w, "Unexpected response format", http.StatusInternalServerError)
			return
		}

		var titles []string
		for _, result := range results {
			titles = append(titles, result.(string))
		}

		// Fetch revisions for the titles
		revisionsURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&prop=revisions&rvprop=content&titles=%s&format=json", url.QueryEscape(strings.Join(titles, "|")))
		revisionsResp, err := http.Get(revisionsURL)
		if err != nil {
			log.Printf("Failed to fetch revisions from Wikimedia API: %v\n", err)
			http.Error(w, "Failed to fetch revisions from Wikimedia API", http.StatusInternalServerError)
			return
		}
		defer revisionsResp.Body.Close()

		revisionsBody, err := io.ReadAll(revisionsResp.Body)
		if err != nil {
			log.Printf("Failed to read revisions response body: %v\n", err)
			http.Error(w, "Failed to read revisions response body", http.StatusInternalServerError)
			return
		}

		if revisionsResp.StatusCode != http.StatusOK {
			log.Printf("Wikimedia API returned non-200 status for revisions: %d\nResponse body: %s\n", revisionsResp.StatusCode, string(revisionsBody))
			http.Error(w, "Failed to fetch revisions from Wikimedia API", http.StatusInternalServerError)
			return
		}

		var revisionsData map[string]interface{}
		if err := json.Unmarshal(revisionsBody, &revisionsData); err != nil {
			log.Printf("Failed to parse revisions JSON response: %v\n", err)
			http.Error(w, "Failed to parse revisions JSON response", http.StatusInternalServerError)
			return
		}

		log.Printf("Revisions data: %v\n", revisionsData)

		// Combine titles and revisions into a response, filtering by "birth_date"
		filteredTitles := []string{}
		if queryPages, ok := revisionsData["query"].(map[string]interface{})["pages"].(map[string]interface{}); ok {
			for _, page := range queryPages {
				if pageMap, ok := page.(map[string]interface{}); ok {
					title, _ := pageMap["title"].(string)
					revisions, _ := pageMap["revisions"].([]interface{})
					for _, revision := range revisions {
						if revisionMap, ok := revision.(map[string]interface{}); ok {
							term := "birth_date"
							if category == "movie" {
								term = "cinematography"
							}
							if content, ok := revisionMap["*"].(string); ok && strings.Contains(content, term) {
								filteredTitles = append(filteredTitles, title)
								break
							}
						}
					}
				}
			}
		}

		response := map[string]interface{}{
			"titles": filteredTitles,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("Failed to encode JSON response: %v\n", err)
			http.Error(w, "Failed to encode JSON response", http.StatusInternalServerError)
		}

		log.Printf("Response: %v\n", response)
	})

	// Start the server
	fmt.Println("Server is running on http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}