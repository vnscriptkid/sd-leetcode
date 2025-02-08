// main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	// Docker SDK packages:

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// ----------
// GORM Models
// ----------

// Problem represents a coding problem.
type Problem struct {
	ID           uint           `gorm:"primaryKey" json:"id"`
	Title        string         `json:"title"`
	Question     string         `json:"question"`
	Level        string         `json:"level"`
	Tags         datatypes.JSON `json:"tags"`         // stored as a JSON array of strings
	CodeStubs    datatypes.JSON `json:"codeStubs"`    // stored as a JSON object: language -> stub
	TestCases    []TestCase     `json:"testCases"`    // one-to-many relation
	FunctionName string         `json:"functionName"` // the name of the function to call
}

// TestCase represents a sample test case for a problem.
type TestCase struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	ProblemID uint           `json:"problemId"`
	Type      string         `json:"type"`
	Input     datatypes.JSON `json:"input"`
	Output    datatypes.JSON `json:"output"`
}

// Submission represents a user submission.
type Submission struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	ProblemID     uint      `json:"problemId"`
	UserID        string    `json:"userId"`
	Code          string    `json:"code"`
	Language      string    `json:"language"`
	CompetitionID string    `json:"competitionId"`
	Passed        bool      `json:"passed"`
	Output        string    `json:"output"`
	Status        string    `json:"status"` // "pending" or "completed"
	CreatedAt     time.Time `json:"createdAt"`
}

// ----------
// Global variables
// ----------

var (
	db              *gorm.DB
	submissionQueue chan uint // channel carrying Submission.ID values for processing
)

// ----------
// Main
// ----------

func main() {
	// Connect to PostgreSQL.
	// Adjust the DSN as appropriate for your setup.
	dsn := "host=localhost user=postgres password=123456 dbname=postgres port=5432 sslmode=disable TimeZone=UTC"
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}

	// Auto-migrate the schema.
	if err := db.AutoMigrate(&Problem{}, &TestCase{}, &Submission{}); err != nil {
		log.Fatalf("failed to migrate: %v", err)
	}

	// Seed sample problems if none exist.
	seedData()

	// Create the submission queue.
	submissionQueue = make(chan uint, 100)

	// Start background worker to process submissions.
	go submissionWorker()

	// Create Gin router.
	router := gin.Default()

	// Load external HTML templates from the templates folder.
	router.LoadHTMLGlob("templates/*")

	// Frontend routes.
	router.GET("/", getIndexPage)
	router.GET("/problem/:id", getProblemPage)

	// API routes.
	api := router.Group("/api")
	{
		api.GET("/problems", apiGetProblems)
		api.GET("/problems/:id", apiGetProblem)
		api.POST("/problems/:id/submit", apiSubmitProblem)
		api.GET("/check/:id", apiCheckSubmission)
		api.GET("/leaderboard/:competitionId", apiLeaderboard)
	}

	// Start the server.
	router.Run(":8080")
}

// ----------
// Frontend Handlers
// ----------

// getIndexPage renders a simple index page with the list of problems.
func getIndexPage(c *gin.Context) {
	var problems []Problem
	if err := db.Find(&problems).Error; err != nil {
		c.String(http.StatusInternalServerError, "Error loading problems")
		return
	}
	c.HTML(http.StatusOK, "index.html", gin.H{
		"Problems": problems,
	})
}

// getProblemPage renders the problem detail page with a submission form.
func getProblemPage(c *gin.Context) {
	id := c.Param("id")
	var problem Problem
	if err := db.Preload("TestCases").First(&problem, id).Error; err != nil {
		c.String(http.StatusNotFound, "Problem not found")
		return
	}
	// Get default code stub for python.
	var stubs map[string]string
	if err := json.Unmarshal(problem.CodeStubs, &stubs); err != nil {
		stubs = map[string]string{"python": ""}
	}
	codeStub := stubs["python"]

	c.HTML(http.StatusOK, "problem.html", gin.H{
		"Problem":  problem,
		"CodeStub": codeStub,
	})
}

// ----------
// API Handlers
// ----------

// apiGetProblems returns a paginated list of problems.
func apiGetProblems(c *gin.Context) {
	pageStr := c.Query("page")
	limitStr := c.Query("limit")
	page, _ := strconv.Atoi(pageStr)
	limit, _ := strconv.Atoi(limitStr)
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 100
	}

	var problems []Problem
	if err := db.Offset((page - 1) * limit).Limit(limit).Find(&problems).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not fetch problems"})
		return
	}
	c.JSON(http.StatusOK, problems)
}

// apiGetProblem returns the full details of a problem.
// Optionally, a query parameter "language" can be used to pick a code stub.
func apiGetProblem(c *gin.Context) {
	id := c.Param("id")
	var problem Problem
	if err := db.Preload("TestCases").First(&problem, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Problem not found"})
		return
	}

	lang := c.Query("language")
	if lang == "" {
		lang = "python"
	}
	var stubs map[string]string
	if err := json.Unmarshal(problem.CodeStubs, &stubs); err != nil {
		stubs = map[string]string{}
	}
	codeStub := stubs[lang]

	c.JSON(http.StatusOK, gin.H{
		"problem":  problem,
		"codeStub": codeStub,
	})
}

// apiSubmitProblem accepts a submission and enqueues it for processing.
func apiSubmitProblem(c *gin.Context) {
	id := c.Param("id")
	// Make sure the problem exists.
	var problem Problem
	if err := db.First(&problem, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Problem not found"})
		return
	}

	// In a real system the user ID would be taken from the session/JWT.
	// Here we take it from a header (or default to "anonymous").
	userID := c.GetHeader("X-User-ID")
	if userID == "" {
		userID = "anonymous"
	}

	var payload struct {
		Code     string `json:"code"`
		Language string `json:"language"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	sub := Submission{
		ProblemID:     problem.ID,
		UserID:        userID,
		Code:          payload.Code,
		Language:      payload.Language,
		CompetitionID: "comp1", // for demo purposes, all submissions belong to "comp1"
		Status:        "pending",
		CreatedAt:     time.Now(),
	}
	if err := db.Create(&sub).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save submission"})
		return
	}

	// Enqueue submission for processing.
	submissionQueue <- sub.ID

	c.JSON(http.StatusOK, gin.H{
		"submissionId": sub.ID,
		"status":       sub.Status,
	})
}

// apiCheckSubmission allows clients to poll for the result of a submission.
func apiCheckSubmission(c *gin.Context) {
	id := c.Param("id")
	var sub Submission
	if err := db.First(&sub, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Submission not found"})
		return
	}
	c.JSON(http.StatusOK, sub)
}

// apiLeaderboard returns a simple leaderboard for a given competition.
func apiLeaderboard(c *gin.Context) {
	competitionID := c.Param("competitionId")
	// Count all submissions that are completed and passed.
	type LBEntry struct {
		UserID    string
		NumSolved int
	}
	rows, err := db.Model(&Submission{}).
		Select("user_id, COUNT(*) as num_solved").
		Where("competition_id = ? AND status = ? AND passed = ?", competitionID, "completed", true).
		Group("user_id").Rows()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error fetching leaderboard"})
		return
	}
	defer rows.Close()
	var leaderboard []LBEntry
	for rows.Next() {
		var entry LBEntry
		if err := rows.Scan(&entry.UserID, &entry.NumSolved); err == nil {
			leaderboard = append(leaderboard, entry)
		}
	}
	c.JSON(http.StatusOK, leaderboard)
}

// ----------
// Background Worker & Containerized Code Execution
// ----------

// submissionWorker processes queued submissions by executing code inside containers.
// submissionWorker processes queued submissions by executing code inside containers.
func submissionWorker() {
	for subID := range submissionQueue {
		// (Optional) Simulate container startup delay.
		time.Sleep(2 * time.Second)

		// Fetch the submission from the database.
		var sub Submission
		if err := db.First(&sub, subID).Error; err != nil {
			continue
		}

		// Process only Python submissions in this demo.
		if sub.Language == "python" {
			// Load the problem along with its test cases.
			var problem Problem
			if err := db.Preload("TestCases").First(&problem, sub.ProblemID).Error; err != nil {
				sub.Status = "completed"
				sub.Output = "Error loading problem"
				sub.Output = strings.ReplaceAll(sub.Output, "\x00", "") // sanitize output
				db.Save(&sub)
				continue
			}

			// Use the function name stored in the problem record.
			functionName := problem.FunctionName
			if functionName == "" {
				functionName = "solve" // default fallback if not set
			}

			// Iterate through each test case.
			allPassed := true
			details := ""
			for i, tc := range problem.TestCases {
				// Unmarshal the test case input.
				var testInput interface{}
				if err := json.Unmarshal(tc.Input, &testInput); err != nil {
					details += fmt.Sprintf("Test case %d: invalid input format.\n", i+1)
					allPassed = false
					continue
				}
				var inputStr string
				switch v := testInput.(type) {
				case string:
					inputStr = v
				default:
					bs, err := json.Marshal(v)
					if err != nil {
						inputStr = ""
					} else {
						inputStr = string(bs)
					}
				}

				// Unmarshal the expected output.
				var expected interface{}
				if err := json.Unmarshal(tc.Output, &expected); err != nil {
					details += fmt.Sprintf("Test case %d: invalid expected output format.\n", i+1)
					allPassed = false
					continue
				}
				var expectedStr string
				switch v := expected.(type) {
				case string:
					expectedStr = v
				default:
					bs, err := json.Marshal(v)
					if err != nil {
						expectedStr = ""
					} else {
						expectedStr = string(bs)
					}
				}

				// Execute the user's code for this test case.
				output, err := executePythonCodeInContainerTest(sub.Code, functionName, inputStr)
				if err != nil {
					details += fmt.Sprintf("Test case %d: execution error: %s\n", i+1, err.Error())
					allPassed = false
					continue
				}

				output = strings.TrimSpace(output)

				// Sanitize the output by removing null bytes.
				output = strings.ReplaceAll(output, "\x00", "")

				if output != expectedStr {
					details += fmt.Sprintf("Test case %d failed: expected `%s`, got `%s`\n", i+1, expectedStr, output)
					allPassed = false
				} else {
					details += fmt.Sprintf("Test case %d passed.\n", i+1)
				}
			}
			if allPassed {
				sub.Passed = true
				sub.Output = "All test cases passed."
			} else {
				sub.Passed = false
				sub.Output = details
			}
			sub.Status = "completed"
			// Sanitize final output before saving to DB.
			sub.Output = strings.ReplaceAll(sub.Output, "\x00", "")
			db.Save(&sub)
		} else {
			// For unsupported languages, simulate a dummy evaluation.
			if sub.Code == "pass" {
				sub.Passed = true
				sub.Output = "Correct Answer"
			} else {
				sub.Passed = false
				sub.Output = "Wrong Answer"
			}
			sub.Status = "completed"
			sub.Output = strings.ReplaceAll(sub.Output, "\x00", "")
			db.Save(&sub)
		}
	}
}

func executePythonCodeInContainerTest(code, functionName, testInput string) (string, error) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", err
	}
	// Wrap the submitted code so multi‑line code is handled properly.
	wrapper := "exec('''" + code + "''')\n"
	// Automatically call the target function with the test input.
	// fmt.Sprintf("%q", testInput) ensures proper quoting.
	wrapper += "print(" + functionName + "(" + fmt.Sprintf("%q", testInput) + "))\n"

	log.Println("Wrapper:", wrapper)

	cmd := []string{"python3", "-c", wrapper}

	// Make sure to run this first: docker pull python:3.8-slim
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: "python:3.8-slim",
		Cmd:   cmd,
		Tty:   true,
	}, nil, nil, nil, "")
	if err != nil {
		return "", err
	}
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return "", err
		}
	case <-statusCh:
	}
	out, err := cli.ContainerLogs(ctx, resp.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", err
	}
	defer out.Close()
	logBytes, err := ioutil.ReadAll(out)
	if err != nil {
		return "", err
	}
	cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	return string(logBytes), nil
}

// ----------
// Helpers
// ----------

// seedData creates sample problems if none exist.
func seedData() {
	var count int64
	db.Model(&Problem{}).Count(&count)
	if count > 0 {
		return
	}

	// Create sample Problem #1: Two Sum.
	tags1, _ := json.Marshal([]string{"array", "hash-table"})
	stubs1, _ := json.Marshal(map[string]string{
		"python":     "def twoSum(nums, target):\n    pass",
		"javascript": "function twoSum(nums, target) {\n    // TODO\n}",
	})
	prob1 := Problem{
		Title:        "Two Sum",
		Question:     "Given an array of integers, return indices of the two numbers such that they add up to a specific target.",
		Level:        "Easy",
		Tags:         datatypes.JSON(tags1),
		CodeStubs:    datatypes.JSON(stubs1),
		FunctionName: "twoSum", // Store the function name to call.
	}
	db.Create(&prob1)

	// Add a test case for problem #1.
	input1, _ := json.Marshal(map[string]interface{}{"nums": []int{2, 7, 11, 15}, "target": 9})
	output1, _ := json.Marshal([]int{0, 1})
	tc1 := TestCase{
		ProblemID: prob1.ID,
		Type:      "default",
		Input:     datatypes.JSON(input1),
		Output:    datatypes.JSON(output1),
	}
	db.Create(&tc1)

	// Create sample Problem #2: Reverse String.
	tags2, _ := json.Marshal([]string{"string", "two-pointers"})
	stubs2, _ := json.Marshal(map[string]string{
		"python":     "def reverseString(s):\n    pass",
		"javascript": "function reverseString(s) {\n    // TODO\n}",
	})
	prob2 := Problem{
		Title:        "Reverse String",
		Question:     "Write a function that reverses a string.",
		Level:        "Easy",
		Tags:         datatypes.JSON(tags2),
		CodeStubs:    datatypes.JSON(stubs2),
		FunctionName: "reverseString", // Store the function name to call.
	}
	db.Create(&prob2)

	// Add a test case for problem #2.
	input2, _ := json.Marshal("hello")
	output2, _ := json.Marshal("olleh")
	tc2 := TestCase{
		ProblemID: prob2.ID,
		Type:      "default",
		Input:     datatypes.JSON(input2),
		Output:    datatypes.JSON(output2),
	}
	db.Create(&tc2)
}
