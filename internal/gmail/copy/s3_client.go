package copy

import (
	"bytes"
	"context"
	"fmt"
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

func buildS3Client(appCfg *config.Config) *s3.Client {
	log.Printf("[DEBUG] [S3] Создание клиента: endpoint=%s, region=%s, bucket=%s",
		appCfg.S3.Endpoint, appCfg.S3.Region, appCfg.S3.Bucket)

	awsCfg, err := awsconfig.LoadDefaultConfig(context.TODO(),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			appCfg.S3.AccessKey,
			appCfg.S3.SecretKey,
			"",
		)),
		awsconfig.WithRegion(appCfg.S3.Region),
	)
	if err != nil {
		log.Printf("[ERROR] [S3] Ошибка создания конфига AWS: %v", err)
		return nil
	}
	log.Printf("[DEBUG] [S3] Конфиг AWS создан успешно")
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(appCfg.S3.Endpoint)
		o.UsePathStyle = true
	})
}

func putObject(ctx context.Context, client *s3.Client, bucket, key string, data []byte) error {
	start := time.Now()
	log.Printf("[DEBUG] [S3-PUT] key=%s size=%d bytes", key, len(data))

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})

	elapsed := time.Since(start).Seconds()
	if err != nil {
		log.Printf("[ERROR] [S3-PUT] key=%s за %.2fs -> %v", key, elapsed, err)
		return err
	}
	log.Printf("[DEBUG] [S3-PUT] key=%s size=%d за %.2fs -> OK", key, len(data), elapsed)
	return nil
}

func getExistingMsgIDs(ctx context.Context, client *s3.Client, bucket, prefix string) (map[string]struct{}, error) {
	start := time.Now()
	log.Printf("[DEBUG] [S3-LIST] bucket=%s prefix=%s", bucket, prefix)

	existing := make(map[string]struct{}, 200000)
	pageNum := 0

	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		pageNum++
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("[ERROR] [S3-LIST] страница %d не удалась: %v", pageNum, err)
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			msgID := extractMsgID(*obj.Key)
			if msgID != "" {
				existing[msgID] = struct{}{}
			}
		}
		log.Printf("[DEBUG] [S3-LIST] страница %d: +%d объектов, итого %d", pageNum, len(page.Contents), len(existing))
	}

	elapsed := time.Since(start).Seconds()
	log.Printf("[DEBUG] [S3-LIST] завершено: %d объектов за %.2fs (%d страниц)", len(existing), elapsed, pageNum)
	return existing, nil
}

func extractMsgID(key string) string {
	base := filepath.Base(key)
	return strings.TrimSuffix(base, ".eml")
}

func diffMissing(gmailIDs []string, existing map[string]struct{}) []string {
	missing := make([]string, 0, len(gmailIDs))
	for _, id := range gmailIDs {
		if _, ok := existing[id]; !ok {
			missing = append(missing, id)
		}
	}
	log.Printf("[DEBUG] [DIFF] Gmail=%d, S3=%d, missing=%d", len(gmailIDs), len(existing), len(missing))
	return missing
}
