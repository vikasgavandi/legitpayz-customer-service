// Customer Service: onboarding + KYC + login
// Run: PORT=8001 go run ./services/customer-service
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/vikasgavandi/legitpayz-customer-service/pkg/auth"
	"github.com/vikasgavandi/legitpayz-customer-service/pkg/awsx"
	"github.com/vikasgavandi/legitpayz-customer-service/pkg/db"
	"github.com/vikasgavandi/legitpayz-customer-service/pkg/events"
	"github.com/vikasgavandi/legitpayz-customer-service/pkg/notify"
	"github.com/vikasgavandi/legitpayz-customer-service/pkg/obs"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func main() {
	database, err := db.Connect()
	if err != nil {
		panic(err)
	}
	defer database.Close()

	r := obs.Engine("customer")

	// Customer onboarding
	r.POST("/customers/onboard", func(c *gin.Context) {
		var req struct {
			FullName string   `json:"full_name" binding:"required"`
			Email    string   `json:"email" binding:"required,email"`
			Phone    string   `json:"phone" binding:"required"`
			PAN      string   `json:"pan" binding:"required"`
			DOB      string   `json:"dob" binding:"required"` // YYYY-MM-DD
			Address  any      `json:"address"`
			KycDocs  []string `json:"kyc_docs"` // S3 URLs uploaded from React via presigned URL
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id := uuid.New().String()
		// Mock credit score: 600 + len(phone)*7 % 250
		score := 600 + (len(req.Email)*13)%250
		addrJSON, _ := json.Marshal(req.Address)
		if string(addrJSON) == "null" {
			addrJSON = []byte("{}")
		}
		docsJSON, _ := json.Marshal(req.KycDocs)
		if string(docsJSON) == "null" {
			docsJSON = []byte("[]")
		}
		_, err = database.Exec(
			`INSERT INTO customers(id,full_name,email,phone,pan,dob,address,kyc_docs,credit_score) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9)`,
			id, req.FullName, req.Email, req.Phone, req.PAN, req.DOB, string(addrJSON), string(docsJSON), score)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_ = events.Default.Publish("customer.events", id, `{"event":"ONBOARDED","id":"`+id+`"}`)
		kycQueue := os.Getenv("KYC_QUEUE_URL")
		if kycQueue == "" {
			kycQueue = "cc-kyc-queue" // resolved via SQS GetQueueUrl on Floci
		}
		_ = notify.Default.Enqueue(kycQueue, `{"customer_id":"`+id+`"}`)
		c.JSON(http.StatusCreated, gin.H{"customer_id": id, "kyc_status": "PENDING", "credit_score": score})
	})

	// KYC decision (internal / backoffice). In prod: Lambda consuming SQS.
	r.POST("/customers/:id/kyc", func(c *gin.Context) {
		var req struct {
			Status string `json:"status" binding:"required"` // VERIFIED | REJECTED
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		_, err := database.Exec(`UPDATE customers SET kyc_status=$1, updated_at=now() WHERE id=$2`, req.Status, c.Param("id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		_ = events.Default.Publish("customer.events", c.Param("id"), `{"event":"KYC_`+req.Status+`"}`)
		_ = notify.Default.PublishSMS("", "KYC "+req.Status)
		c.JSON(200, gin.H{"ok": true})
	})

	r.GET("/customers/:id", func(c *gin.Context) {
		var out map[string]any
		row := database.QueryRow(`SELECT id,full_name,email,phone,kyc_status,credit_score,created_at FROM customers WHERE id=$1`, c.Param("id"))
		var id, name, email, phone, kyc string
		var score int
		var created string
		if err := row.Scan(&id, &name, &email, &phone, &kyc, &score, &created); err != nil {
			c.JSON(404, gin.H{"error": "not found"})
			return
		}
		out = map[string]any{"id": id, "full_name": name, "email": email, "phone": phone, "kyc_status": kyc, "credit_score": score, "created_at": created}
		c.JSON(200, out)
	})

	// Simplified login: email -> JWT (prod: OTP via SNS + Cognito)
	r.POST("/login", func(c *gin.Context) {
		var req struct {
			Email string `json:"email" binding:"required,email"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var id string
		if err := database.QueryRow(`SELECT id FROM customers WHERE email=$1`, req.Email).Scan(&id); err != nil {
			c.JSON(401, gin.H{"error": "unknown email"})
			return
		}
		tok, _ := auth.Issue(id)
		c.JSON(200, gin.H{"token": tok, "customer_id": id})
	})

	// S3 presigned PUT for KYC docs (bucket cc-kyc on Floci/S3)
	r.POST("/customers/kyc-upload-url", func(c *gin.Context) {
		var req struct {
			Filename    string `json:"filename" binding:"required"`
			ContentType string `json:"content_type"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if !awsx.Enabled() {
			c.JSON(501, gin.H{"error": "S3 not configured (set AWS_ENDPOINT_URL)"})
			return
		}
		bucket := os.Getenv("KYC_BUCKET")
		if bucket == "" {
			bucket = "cc-kyc"
		}
		key := "kyc/" + uuid.New().String() + "-" + req.Filename
		ctx := context.Background()
		s3c, err := awsx.S3(ctx)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		ps := s3.NewPresignClient(s3c)
		in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
		if req.ContentType != "" {
			in.ContentType = aws.String(req.ContentType)
		}
		out, err := ps.PresignPutObject(ctx, in, s3.WithPresignExpires(15*time.Minute))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"upload_url": out.URL, "s3_key": key, "bucket": bucket})
	})

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true, "svc": "customer"}) })

	port := os.Getenv("PORT")
	if port == "" {
		port = "8001"
	}
	_ = r.Run(":" + port)
}
