///usr/bin/env jbang "$0" "$@" ; exit $?
//DEPS org.testcontainers:testcontainers-minio:2.0.5
//DEPS software.amazon.awssdk:s3:2.55.12
//JAVA 17+
// The README's Testcontainers snippet, run as written: MinIOContainer with the image substituted.
// usage: jbang TestcontainersCheck.java ghcr.io/midagedev/ss33:0.4
import java.net.URI;
import org.testcontainers.containers.MinIOContainer;
import org.testcontainers.utility.DockerImageName;
import software.amazon.awssdk.auth.credentials.*;
import software.amazon.awssdk.core.sync.RequestBody;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3Client;

public class TestcontainersCheck {
  public static void main(String[] a) {
    try (var minio = new MinIOContainer(DockerImageName.parse(a[0]).asCompatibleSubstituteFor("minio/minio"))) {
      minio.start();
      var s3 = S3Client.builder().endpointOverride(URI.create(minio.getS3URL())).region(Region.US_EAST_1).forcePathStyle(true)
        .credentialsProvider(StaticCredentialsProvider.create(AwsBasicCredentials.create(minio.getUserName(), minio.getPassword()))).build();
      s3.createBucket(r -> r.bucket("tc-java"));
      s3.putObject(r -> r.bucket("tc-java").key("k"), RequestBody.fromString("hello"));
      String got = s3.getObjectAsBytes(r -> r.bucket("tc-java").key("k")).asUtf8String();
      if (!got.equals("hello")) throw new AssertionError("got " + got);
      System.out.println("Testcontainers MinIOContainer with " + a[0] + ": ALL PASS");
    }
  }
}
