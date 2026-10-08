package server

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/midagedev/ss33/internal/store"
)

// Bucket notifications, the MinIO way: webhook targets come from MINIO_NOTIFY_WEBHOOK_* settings, a bucket's
// NotificationConfiguration routes events to them by ARN (arn:minio:sqs::<id>:webhook, what `mc event add`
// writes), and each matching event is POSTed as S3 event JSON in MinIO's envelope.

// Webhook is one notification target.
type Webhook struct {
	Endpoint  string
	AuthToken string // sent as "Authorization: Bearer <token>" unless it already names a scheme
}

type notificationConfig struct {
	XMLName xml.Name `xml:"NotificationConfiguration"`
	Queues  []struct {
		ID     string   `xml:"Id"`
		ARN    string   `xml:"Queue"`
		Events []string `xml:"Event"`
		Rules  []struct {
			Name  string
			Value string
		} `xml:"Filter>S3Key>FilterRule"`
	} `xml:"QueueConfiguration"`
	Topics    []struct{} `xml:"TopicConfiguration"`
	Functions []struct{} `xml:"CloudFunctionConfiguration"`
}

// webhookID returns the target id an ARN names: arn:minio:sqs:<region>:<id>:webhook.
func webhookID(arn string) (string, bool) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "minio" || parts[2] != "sqs" || parts[5] != "webhook" {
		return "", false
	}
	return parts[4], true
}

// checkNotificationConfig refuses destinations ss33 cannot deliver to, as S3 refuses ones it cannot
// validate: SNS and Lambda, AWS queues, and webhooks that are not configured.
func (s *Server) checkNotificationConfig(body []byte) error {
	var cfg notificationConfig
	if err := xml.Unmarshal(body, &cfg); err != nil {
		return err
	}
	if len(cfg.Topics)+len(cfg.Functions) > 0 {
		return fmt.Errorf("only webhook queues (arn:minio:sqs::<id>:webhook) can be notified")
	}
	for _, q := range cfg.Queues {
		id, ok := webhookID(q.ARN)
		if _, configured := s.Webhooks[id]; !ok || !configured {
			return fmt.Errorf("unable to validate the destination %s: no webhook target %q is configured (MINIO_NOTIFY_WEBHOOK_ENDPOINT_%s)", q.ARN, id, id)
		}
	}
	return nil
}

// notify sends event (e.g. "s3:ObjectCreated:Put") about meta to every target the bucket routes it to.
// Delivery is asynchronous and ordered per target.
func (s *Server) notify(r *http.Request, bucket, event string, meta store.ObjectMeta) {
	if len(s.Webhooks) == 0 {
		return
	}
	b, err := s.Store.Bucket(bucket)
	if err != nil || b.Configs["notification"] == "" {
		return
	}
	var cfg notificationConfig
	if xml.Unmarshal([]byte(b.Configs["notification"]), &cfg) != nil {
		return
	}
	for _, q := range cfg.Queues {
		id, _ := webhookID(q.ARN)
		target, ok := s.Webhooks[id]
		if !ok || !eventMatches(q.Events, event) || !keyMatches(q.Rules, meta.Key) {
			continue
		}
		s.webhookQueue(id, target) <- eventPayload(r, s.Region, s.Creds.AccessKey, bucket, event, q.ID, meta)
	}
}

func eventMatches(patterns []string, event string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, event); ok || p == event {
			return true
		}
	}
	return false
}

func keyMatches(rules []struct{ Name, Value string }, key string) bool {
	for _, r := range rules {
		switch strings.ToLower(r.Name) {
		case "prefix":
			if !strings.HasPrefix(key, r.Value) {
				return false
			}
		case "suffix":
			if !strings.HasSuffix(key, r.Value) {
				return false
			}
		}
	}
	return true
}

// eventPayload is the JSON MinIO posts to webhooks: S3's event Records, wrapped with EventName and Key.
func eventPayload(r *http.Request, region, accessKey, bucket, event, configID string, meta store.ObjectMeta) []byte {
	now := time.Now().UTC()
	object := map[string]any{
		"key":       url.QueryEscape(meta.Key),
		"sequencer": fmt.Sprintf("%016X", now.UnixNano()),
	}
	if !strings.HasPrefix(event, "s3:ObjectRemoved:") {
		object["size"], object["eTag"], object["contentType"] = meta.Size, strings.Trim(meta.ETag, `"`), meta.ContentType
		if len(meta.UserMeta) > 0 {
			userMeta := map[string]string{}
			for k, v := range meta.UserMeta {
				userMeta["X-Amz-Meta-"+k] = v
			}
			object["userMetadata"] = userMeta
		}
	}
	if meta.VersionID != "" {
		object["versionId"] = meta.VersionID
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	record := map[string]any{
		"eventVersion":      "2.0",
		"eventSource":       "minio:s3",
		"awsRegion":         region,
		"eventTime":         now.Format("2006-01-02T15:04:05.000Z"),
		"eventName":         event,
		"userIdentity":      map[string]string{"principalId": accessKey},
		"requestParameters": map[string]string{"principalId": accessKey, "region": region, "sourceIPAddress": host},
		"responseElements":  map[string]string{"x-amz-request-id": requestID()},
		"s3": map[string]any{
			"s3SchemaVersion": "1.0",
			"configurationId": configID,
			"bucket":          map[string]any{"name": bucket, "ownerIdentity": map[string]string{"principalId": accessKey}, "arn": "arn:aws:s3:::" + bucket},
			"object":          object,
		},
	}
	body, _ := json.Marshal(map[string]any{"EventName": event, "Key": bucket + "/" + meta.Key, "Records": []any{record}})
	return body
}

// webhookQueue returns the ordered delivery queue of one target, starting its sender on first use.
// ponytail: one goroutine and a 1024-event buffer per target; events beyond that wait for the sender.
func (s *Server) webhookQueue(id string, target Webhook) chan []byte {
	s.queuesMu.Lock()
	defer s.queuesMu.Unlock()
	if s.queues == nil {
		s.queues = map[string]chan []byte{}
	}
	q, ok := s.queues[id]
	if !ok {
		q = make(chan []byte, 1024)
		s.queues[id] = q
		go s.deliver(id, target, q)
	}
	return q
}

// deliver posts each event, retrying a failed delivery a few times before dropping it.
func (s *Server) deliver(id string, target Webhook, q chan []byte) {
	client := &http.Client{Timeout: 10 * time.Second}
	for body := range q {
		for attempt := 0; attempt < 5; attempt++ {
			req, _ := http.NewRequest(http.MethodPost, target.Endpoint, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if t := target.AuthToken; t != "" {
				if !strings.Contains(t, " ") {
					t = "Bearer " + t
				}
				req.Header.Set("Authorization", t)
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode < 300 {
					break
				}
				err = fmt.Errorf("%s", resp.Status)
			}
			if s.Log != nil {
				s.Log.Warn("webhook delivery failed", "target", id, "attempt", attempt+1, "err", err)
			}
			time.Sleep(time.Duration(200<<attempt) * time.Millisecond)
		}
	}
}

// notifyDelete reports a delete: a new delete marker, or bytes (or a marker) removed for good.
func (s *Server) notifyDelete(r *http.Request, bucket, key string, d store.Deleted, versioned bool) {
	event := "s3:ObjectRemoved:Delete"
	if d.DeleteMarker && !versioned {
		event = "s3:ObjectRemoved:DeleteMarkerCreated"
	}
	meta := store.ObjectMeta{Key: key}
	if d.VersionID != "null" {
		meta.VersionID = d.VersionID
	}
	s.notify(r, bucket, event, meta)
}

// WebhooksFromEnv reads MinIO's webhook target settings: MINIO_NOTIFY_WEBHOOK_ENABLE_<ID>=on with
// MINIO_NOTIFY_WEBHOOK_ENDPOINT_<ID> and optionally MINIO_NOTIFY_WEBHOOK_AUTH_TOKEN_<ID>. The variables
// without a suffix define the target "_", as in MinIO.
func WebhooksFromEnv(environ []string) map[string]Webhook {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	const prefix = "MINIO_NOTIFY_WEBHOOK_"
	targets := map[string]Webhook{}
	for k, v := range env {
		rest, ok := strings.CutPrefix(k, prefix+"ENABLE")
		if !ok || (rest != "" && !strings.HasPrefix(rest, "_")) || !(strings.EqualFold(v, "on") || strings.EqualFold(v, "true")) {
			continue
		}
		id := strings.TrimPrefix(rest, "_")
		if id == "" {
			id = "_"
		}
		if endpoint := env[prefix+"ENDPOINT"+rest]; endpoint != "" {
			targets[id] = Webhook{Endpoint: endpoint, AuthToken: env[prefix+"AUTH_TOKEN"+rest]}
		}
	}
	return targets
}
