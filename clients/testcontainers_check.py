"""testcontainers-python's MinioContainer with the image substituted, as the README suggests.

usage: python testcontainers_check.py ghcr.io/midagedev/ss33:0.3
"""
import io
import sys

from testcontainers.community.minio import MinioContainer

with MinioContainer(image=sys.argv[1]) as minio:
    client = minio.get_client()  # the minio Python SDK, as the module hands it out
    client.make_bucket("tc-python")
    client.put_object("tc-python", "k", io.BytesIO(b"hello"), 5)
    got = client.get_object("tc-python", "k").read()
    assert got == b"hello", got
print(f"testcontainers MinioContainer with {sys.argv[1]}: ALL PASS")
