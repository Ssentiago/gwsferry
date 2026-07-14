package importyandex

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gwsferry/internal/shared/config"
)

// EmailMeta — метаданные письма в S3 (ID + путь к объекту).
type EmailMeta struct {
	MessageID string // ID письма (имя файла без .eml)
	Key       string // полный S3-ключ объекта
	Size      int64  // размер в байтах
}

// S3Reader — интерфейс для чтения писем из S3.
// S3Client и MockS3Client реализуют этот интерфейс.
type S3Reader interface {
	ListEmails(ctx context.Context, email string) ([]EmailMeta, error)
	GetEmail(ctx context.Context, key string) ([]byte, error)
}

// S3Client — обёртка над AWS S3 SDK для чтения .eml файлов.
// Только чтение: List, Get, Exists. Никаких Put/Delete.
type S3Client struct {
	client *s3.Client
	bucket string
	prefix string // базовый префикс, например "ru/user/gmail"
}

// NewS3Client создаёт S3-клиент из конфига приложения.
func NewS3Client(cfg *config.Config) (*S3Client, error) {
	log.Printf("[INFO] [S3] создание клиента: endpoint=%s region=%s bucket=%s prefix=ru/user/gmail",
		cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket)

	awsCfg, err := awsconfig.LoadDefaultConfig(context.TODO(),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.S3.AccessKey,
			cfg.S3.SecretKey,
			"",
		)),
		awsconfig.WithRegion(cfg.S3.Region),
	)
	if err != nil {
		log.Printf("[ERROR] [S3] ошибка конфигурации AWS SDK: %v", err)
		return nil, fmt.Errorf("s3 config: %w", err)
	}
	log.Printf("[DEBUG] [S3] AWS SDK config loaded OK")

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.S3.Endpoint)
		o.UsePathStyle = true
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	log.Printf("[INFO] [S3] клиент создан OK")
	return &S3Client{
		client: client,
		bucket: cfg.S3.Bucket,
		prefix: "ru/user/gmail",
	}, nil
}

// UserPrefix возвращает префикс для конкретного юзера: ru/users/{email}/gmail/
func (s *S3Client) UserPrefix(email string) string {
	return fmt.Sprintf("ru/users/%s/gmail/", email)
}

// ListEmails возвращает список .eml файлов в S3 для указанного юзера.
// Ключи формата ru/users/{email}/gmail/{message_id}.eml
func (s *S3Client) ListEmails(ctx context.Context, email string) ([]EmailMeta, error) {
	prefix := s.UserPrefix(email)
	start := time.Now()
	log.Printf("[DEBUG] [S3-LIST] bucket=%s prefix=%s", s.bucket, prefix)

	var result []EmailMeta
	pageNum := 0

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		pageNum++
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("[ERROR] [S3-LIST] страница %d: %v", pageNum, err)
			return nil, fmt.Errorf("list page %d: %w", pageNum, err)
		}
		for _, obj := range page.Contents {
			id := extractEmailID(*obj.Key)
			if id == "" {
				continue
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			result = append(result, EmailMeta{
				MessageID: id,
				Key:       *obj.Key,
				Size:      size,
			})
		}
		log.Printf("[DEBUG] [S3-LIST] страница %d: +%d объектов", pageNum, len(page.Contents))
	}

	elapsed := time.Since(start).Seconds()
	log.Printf("[DEBUG] [S3-LIST] email=%s: %d emails за %.2fs", email, len(result), elapsed)
	return result, nil
}

// GetEmail скачивает содержимое .eml файла из S3.
func (s *S3Client) GetEmail(ctx context.Context, key string) ([]byte, error) {
	start := time.Now()
	log.Printf("[DEBUG] [S3-GET] key=%s", key)

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("[ERROR] [S3-GET] key=%s за %.2fs -> %v", key, time.Since(start).Seconds(), err)
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}

	elapsed := time.Since(start).Seconds()
	log.Printf("[DEBUG] [S3-GET] key=%s size=%d за %.2fs -> OK", key, len(data), elapsed)
	return data, nil
}

// EmailExists проверяет, существует ли .eml файл в S3.
func (s *S3Client) EmailExists(ctx context.Context, key string) (bool, error) {
	log.Printf("[DEBUG] [S3-EXISTS] key=%s", key)
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// HeadObject возвращает ошибку 404 если объект не найден
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			log.Printf("[DEBUG] [S3-EXISTS] key=%s → NOT FOUND", key)
			return false, nil
		}
		log.Printf("[ERROR] [S3-EXISTS] key=%s: %v", key, err)
		return false, fmt.Errorf("head %s: %w", key, err)
	}
	log.Printf("[DEBUG] [S3-EXISTS] key=%s → EXISTS", key)
	return true, nil
}

// extractEmailID извлекает message_id из S3-ключа.
// Формат: {prefix}/{uid}/messages/{message_id}.eml → message_id
func extractEmailID(key string) string {
	base := filepath.Base(key)
	return strings.TrimSuffix(base, ".eml")
}
