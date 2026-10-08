"""boto3 against an S3 endpoint: the calls Python apps and test suites typically make.

usage: python boto3_check.py http://127.0.0.1:9000   (credentials minioadmin / minioadmin)
"""
import io
import sys
import urllib.request
import uuid

import boto3
from botocore.config import Config

ep = sys.argv[1]
fails = 0


def check(name, fn):
    global fails
    try:
        fn()
        print("PASS", name)
    except Exception as e:
        fails += 1
        print("FAIL", name, "->", type(e).__name__, str(e)[:300])


def client(**cfg):
    return boto3.client("s3", endpoint_url=ep, aws_access_key_id="minioadmin", aws_secret_access_key="minioadmin",
                        region_name="us-east-1", config=Config(**cfg))


s3 = client(s3={"addressing_style": "path"})
B = "clients-boto3"


def eq(got, want):
    assert got == want, f"want {want!r}, got {got!r}"


check("create_bucket", lambda: s3.create_bucket(Bucket=B))
check("put_object", lambda: s3.put_object(Bucket=B, Key="a.txt", Body=b"hello", ContentType="text/plain",
                                          Metadata={"owner": "me"}))


def head():
    h = s3.head_object(Bucket=B, Key="a.txt")
    eq(h["ContentLength"], 5)
    eq(h["Metadata"], {"owner": "me"})


check("head_object + metadata", head)
check("get_object", lambda: eq(s3.get_object(Bucket=B, Key="a.txt")["Body"].read(), b"hello"))
check("get_object Range", lambda: eq(s3.get_object(Bucket=B, Key="a.txt", Range="bytes=1-3")["Body"].read(), b"ell"))

data = bytes(range(256)) * 40000  # ~10 MB: upload_fileobj switches to threaded multipart


def transfer():
    s3.upload_fileobj(io.BytesIO(data), B, "big.bin")
    got = io.BytesIO()
    s3.download_fileobj(B, "big.bin", got)
    assert got.getvalue() == data


check("upload_fileobj / download_fileobj (multipart)", transfer)


def listing():
    for i in range(5):
        s3.put_object(Bucket=B, Key=f"dir/k{i}", Body=b"")
    pages = s3.get_paginator("list_objects_v2").paginate(Bucket=B, Prefix="dir/", PaginationConfig={"PageSize": 2})
    eq([o["Key"] for p in pages for o in p.get("Contents", [])], [f"dir/k{i}" for i in range(5)])


check("list_objects_v2 paginator", listing)
check("copy_object", lambda: s3.copy_object(Bucket=B, Key="a-copy.txt", CopySource={"Bucket": B, "Key": "a.txt"}))
check("copy (managed, multipart source)", lambda: s3.copy({"Bucket": B, "Key": "big.bin"}, B, "big-copy.bin"))


def presign():
    url = s3.generate_presigned_url("get_object", Params={"Bucket": B, "Key": "a.txt"}, ExpiresIn=60)
    eq(urllib.request.urlopen(url).read(), b"hello")


check("generate_presigned_url GET", presign)


def presign_v4():
    v4 = client(s3={"addressing_style": "path"}, signature_version="s3v4")
    url = v4.generate_presigned_url("get_object", Params={"Bucket": B, "Key": "a.txt"}, ExpiresIn=60)
    eq(urllib.request.urlopen(url).read(), b"hello")


check("generate_presigned_url GET (s3v4)", presign_v4)


def presign_post():
    p = s3.generate_presigned_post(B, "form/${filename}", ExpiresIn=60)
    boundary = uuid.uuid4().hex
    body = b"".join(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
                    for k, v in p["fields"].items())
    body += (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="f.txt"\r\n'
             f"Content-Type: text/plain\r\n\r\nform-data\r\n--{boundary}--\r\n").encode()
    req = urllib.request.Request(p["url"], data=body,
                                 headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
    urllib.request.urlopen(req)
    eq(s3.get_object(Bucket=B, Key="form/f.txt")["Body"].read(), b"form-data")


check("generate_presigned_post (browser form upload)", presign_post)


def public_acl():
    s3.put_object(Bucket=B, Key="pub.txt", Body=b"pub", ACL="public-read")
    eq(urllib.request.urlopen(f"{ep}/{B}/pub.txt").read(), b"pub")


check("put_object ACL=public-read, then anonymous GET", public_acl)


def tagging():
    s3.put_object_tagging(Bucket=B, Key="a.txt", Tagging={"TagSet": [{"Key": "k", "Value": "v"}]})
    eq(s3.get_object_tagging(Bucket=B, Key="a.txt")["TagSet"], [{"Key": "k", "Value": "v"}])


check("put/get_object_tagging", tagging)


def if_none_match():
    try:
        s3.put_object(Bucket=B, Key="a.txt", Body=b"x", IfNoneMatch="*")
    except s3.exceptions.ClientError as e:
        eq(e.response["Error"]["Code"], "PreconditionFailed")
        return
    raise AssertionError("overwrite was accepted")


check("put_object IfNoneMatch='*' refuses an overwrite", if_none_match)


def missing():
    try:
        s3.get_object(Bucket=B, Key="nope")
    except s3.exceptions.NoSuchKey:
        return
    raise AssertionError("no NoSuchKey")


check("get_object missing key raises NoSuchKey", missing)
check("put_bucket_cors", lambda: s3.put_bucket_cors(
    Bucket=B, CORSConfiguration={"CORSRules": [{"AllowedOrigins": ["*"], "AllowedMethods": ["GET", "PUT"]}]}))
check("list_multipart_uploads", lambda: s3.list_multipart_uploads(Bucket=B))
check("list_object_versions", lambda: s3.list_object_versions(Bucket=B))
check("get_bucket_versioning", lambda: s3.get_bucket_versioning(Bucket=B))
check("default addressing (no addressing_style)",
      lambda: client().put_object(Bucket=B, Key="default.txt", Body=b"default"))


def delete_all():
    keys = [{"Key": o["Key"]} for o in s3.list_objects_v2(Bucket=B).get("Contents", [])]
    s3.delete_objects(Bucket=B, Delete={"Objects": keys})
    s3.delete_bucket(Bucket=B)


check("delete_objects + delete_bucket", delete_all)
print(f"boto3 {boto3.__version__}:", "ALL PASS" if not fails else f"{fails} FAILED")
sys.exit(1 if fails else 0)
