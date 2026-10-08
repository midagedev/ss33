///usr/bin/env jbang "$0" "$@" ; exit $?
//DEPS software.amazon.awssdk:s3:2.55.12
//JAVA 17+
// AWS SDK for Java v2 against an S3 endpoint: sync and async (multipart) clients and the presigner.
// usage: jbang JavaCheck.java http://127.0.0.1:9000   (credentials minioadmin / minioadmin)
import java.io.*;
import java.net.URI;
import java.net.http.*;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.*;
import software.amazon.awssdk.auth.credentials.*;
import software.amazon.awssdk.core.async.AsyncRequestBody;
import software.amazon.awssdk.core.sync.RequestBody;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.*;
import software.amazon.awssdk.services.s3.model.*;
import software.amazon.awssdk.services.s3.presigner.S3Presigner;
import software.amazon.awssdk.services.s3.presigner.model.*;

public class JavaCheck {
  static int fails = 0;
  static void check(String name, Runnable r) {
    try { r.run(); System.out.println("PASS " + name); }
    catch (Throwable t) { fails++; System.out.println("FAIL " + name + ": " + t); Throwable c = t.getCause(); while (c != null) { System.out.println("     cause: " + c); c = c.getCause(); } }
  }
  static void eq(Object a, Object b) { if (!Objects.equals(a, b)) throw new AssertionError("want " + b + " got " + a); }
  static byte[] rnd(int n) { byte[] b = new byte[n]; new Random(n).nextBytes(b); return b; }

  public static void main(String[] a) throws Exception {
    URI ep = URI.create(a[0]);
    var creds = StaticCredentialsProvider.create(AwsBasicCredentials.create("minioadmin", "minioadmin"));
    var cfg = S3Configuration.builder().pathStyleAccessEnabled(true).build();
    Region region = Region.of(a.length > 1 ? a[1] : "us-east-1");
    S3Client s3 = S3Client.builder().region(region).credentialsProvider(creds).endpointOverride(ep).serviceConfiguration(cfg).build();
    S3AsyncClient as3 = S3AsyncClient.builder().region(region).credentialsProvider(creds).endpointOverride(ep).serviceConfiguration(cfg).multipartEnabled(true).build();
    S3Presigner pre = S3Presigner.builder().region(region).credentialsProvider(creds).endpointOverride(ep).serviceConfiguration(cfg).build();
    HttpClient http = HttpClient.newHttpClient();
    String b = "clients-java";

    check("createBucket", () -> { try { s3.createBucket(r -> r.bucket(b)); } catch (BucketAlreadyOwnedByYouException e) {} });
    byte[] small = "hello java sdk".getBytes(StandardCharsets.UTF_8);
    check("putObject sync fromInputStream", () ->
      s3.putObject(PutObjectRequest.builder().bucket(b).key("dir/small.txt").contentType("text/plain").build(),
        RequestBody.fromInputStream(new ByteArrayInputStream(small), small.length)));
    check("putObject checksum SHA256", () -> {
      var resp = s3.putObject(PutObjectRequest.builder().bucket(b).key("dir/sha.bin").checksumAlgorithm(ChecksumAlgorithm.SHA256).build(),
        RequestBody.fromInputStream(new ByteArrayInputStream(small), small.length));
      try { eq(resp.checksumSHA256(), Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-256").digest(small))); }
      catch (java.security.NoSuchAlgorithmException e) { throw new RuntimeException(e); }
    });
    byte[] big = rnd(20 * 1024 * 1024 + 123);
    check("async multipart putObject 20MiB", () ->
      as3.putObject(PutObjectRequest.builder().bucket(b).key("dir/big.bin").contentType("application/octet-stream").build(),
        AsyncRequestBody.fromInputStream(new ByteArrayInputStream(big), (long) big.length, java.util.concurrent.Executors.newFixedThreadPool(2))).join());
    check("headObject", () -> { var h = s3.headObject(r -> r.bucket(b).key("dir/small.txt")); eq(h.contentLength(), (long) small.length); eq(h.contentType(), "text/plain"); });
    check("getObjectAsBytes small", () -> eq(new String(s3.getObjectAsBytes(r -> r.bucket(b).key("dir/small.txt")).asByteArray(), StandardCharsets.UTF_8), "hello java sdk"));
    check("getObject stream big", () -> { try (var in = s3.getObject(r -> r.bucket(b).key("dir/big.bin"))) { eq(Arrays.equals(in.readAllBytes(), big), true); } catch (IOException e) { throw new UncheckedIOException(e); } });
    check("listObjectsV2 paginated", () -> {
      for (int i = 0; i < 5; i++) { int j = i; s3.putObject(r -> r.bucket(b).key("list/k" + j), RequestBody.fromString("x")); }
      List<String> keys = new ArrayList<>(); String tok = null;
      do { String t = tok; var r = s3.listObjectsV2(q -> q.bucket(b).prefix("list/").maxKeys(2).continuationToken(t)); r.contents().forEach(o -> keys.add(o.key())); tok = r.isTruncated() ? r.nextContinuationToken() : null; } while (tok != null);
      eq(keys, List.of("list/k0", "list/k1", "list/k2", "list/k3", "list/k4"));
    });
    check("presigned GET with response overrides", () -> {
      String url = pre.presignGetObject(GetObjectPresignRequest.builder().signatureDuration(Duration.ofMinutes(5))
        .getObjectRequest(GetObjectRequest.builder().bucket(b).key("dir/small.txt").responseCacheControl("max-age=60").responseContentDisposition("attachment; filename=\"a b.txt\"").build()).build()).url().toString();
      var r = send(http, HttpRequest.newBuilder(URI.create(url)).header("Origin", "https://app.example.com").GET().build());
      eq(r.statusCode(), 200); eq(new String(r.body()), "hello java sdk");
      eq(r.headers().firstValue("Cache-Control").orElse(null), "max-age=60");
      eq(r.headers().firstValue("Content-Disposition").orElse(null), "attachment; filename=\"a b.txt\"");
    });
    check("presigned PUT (unsigned length)", () -> {
      String url = pre.presignPutObject(PutObjectPresignRequest.builder().signatureDuration(Duration.ofMinutes(5)).putObjectRequest(PutObjectRequest.builder().bucket(b).key("up/free.bin").build()).build()).url().toString();
      var r = send(http, HttpRequest.newBuilder(URI.create(url)).PUT(HttpRequest.BodyPublishers.ofByteArray(small)).build());
      eq(r.statusCode(), 200);
    });
    check("presigned PUT signed content-length", () -> {
      String url = pre.presignPutObject(PutObjectPresignRequest.builder().signatureDuration(Duration.ofMinutes(5)).putObjectRequest(PutObjectRequest.builder().bucket(b).key("up/len.bin").contentLength((long) small.length).build()).build()).url().toString();
      var ok = send(http, HttpRequest.newBuilder(URI.create(url)).PUT(HttpRequest.BodyPublishers.ofByteArray(small)).build());
      eq(ok.statusCode(), 200);
      var bad = send(http, HttpRequest.newBuilder(URI.create(url)).PUT(HttpRequest.BodyPublishers.ofByteArray("short".getBytes())).build());
      eq(bad.statusCode(), 403);
    });
    check("browser multipart via presigned UploadPart", () -> {
      String key = "mp/browser.bin";
      String id = s3.createMultipartUpload(r -> r.bucket(b).key(key).contentType("application/x-custom")).uploadId();
      byte[] p1 = rnd(5 * 1024 * 1024), p2 = rnd(1000);
      for (int n = 1; n <= 2; n++) {
        int pn = n;
        String url = pre.presignUploadPart(UploadPartPresignRequest.builder().signatureDuration(Duration.ofMinutes(5)).uploadPartRequest(UploadPartRequest.builder().bucket(b).key(key).uploadId(id).partNumber(pn).build()).build()).url().toString();
        var r = send(http, HttpRequest.newBuilder(URI.create(url)).header("Origin", "https://app.example.com").PUT(HttpRequest.BodyPublishers.ofByteArray(pn == 1 ? p1 : p2)).build());
        eq(r.statusCode(), 200);
        if (!r.headers().firstValue("Access-Control-Expose-Headers").orElse("").contains("ETag")) throw new AssertionError("ETag not exposed to browser");
      }
      var parts = new ArrayList<Part>(); s3.listPartsPaginator(r -> r.bucket(b).key(key).uploadId(id).maxParts(1)).parts().forEach(parts::add);
      eq(parts.size(), 2); eq(parts.get(0).size(), (long) p1.length);
      s3.completeMultipartUpload(r -> r.bucket(b).key(key).uploadId(id).multipartUpload(m -> m.parts(parts.stream().map(p -> CompletedPart.builder().partNumber(p.partNumber()).eTag(p.eTag()).build()).toList())));
      var h = s3.headObject(r -> r.bucket(b).key(key)); eq(h.contentLength(), (long) (p1.length + p2.length)); eq(h.contentType(), "application/x-custom");
    });
    check("anonymous GET after public policy", () -> {
      s3.putBucketPolicy(r -> r.bucket(b).policy("{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":[\"*\"]},\"Action\":[\"s3:GetObject\"],\"Resource\":[\"arn:aws:s3:::" + b + "/*\"]}]}"));
      eq(send(http, HttpRequest.newBuilder(ep.resolve("/" + b + "/dir/small.txt")).GET().build()).statusCode(), 200);
    });
    check("deleteObject + NoSuchKey", () -> {
      s3.deleteObject(r -> r.bucket(b).key("dir/small.txt"));
      try { s3.headObject(r -> r.bucket(b).key("dir/small.txt")); throw new AssertionError("still there"); } catch (NoSuchKeyException e) {}
      try { s3.getObjectAsBytes(r -> r.bucket(b).key("dir/small.txt")); throw new AssertionError("still there"); } catch (NoSuchKeyException e) {}
    });
    System.out.println("aws-sdk-java-v2 " + software.amazon.awssdk.core.util.VersionInfo.SDK_VERSION + ": " + (fails == 0 ? "ALL PASS" : fails + " FAILED"));
    System.exit(fails == 0 ? 0 : 1);
  }
  static HttpResponse<byte[]> send(HttpClient h, HttpRequest r) { try { return h.send(r, HttpResponse.BodyHandlers.ofByteArray()); } catch (Exception e) { throw new RuntimeException(e); } }
}
