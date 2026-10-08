# The Terraform AWS provider against an S3 endpoint: a bucket with the configuration resources teams
# usually attach to it. CI runs apply, then plan (must show no changes), then destroy.
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.68.0" }
  }
}

variable "endpoint" { type = string }

provider "aws" {
  region                      = "us-east-1"
  access_key                  = "minioadmin"
  secret_key                  = "minioadmin"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  s3_use_path_style           = true
  endpoints { s3 = var.endpoint }
}

resource "aws_s3_bucket" "b" {
  bucket        = "clients-terraform"
  force_destroy = true
  tags          = { team = "platform" }
}

resource "aws_s3_bucket_versioning" "b" {
  bucket = aws_s3_bucket.b.id
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "b" {
  bucket = aws_s3_bucket.b.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "b" {
  bucket = aws_s3_bucket.b.id
  rule {
    id     = "expire-tmp"
    status = "Enabled"
    filter { prefix = "tmp/" }
    expiration { days = 7 }
  }
}

resource "aws_s3_bucket_cors_configuration" "b" {
  bucket = aws_s3_bucket.b.id
  cors_rule {
    allowed_methods = ["GET", "PUT"]
    allowed_origins = ["https://app.example.com"]
  }
}

resource "aws_s3_bucket_public_access_block" "b" {
  bucket                  = aws_s3_bucket.b.id
  block_public_acls       = false
  block_public_policy     = false
  ignore_public_acls      = false
  restrict_public_buckets = false
}

resource "aws_s3_bucket_ownership_controls" "b" {
  bucket = aws_s3_bucket.b.id
  rule { object_ownership = "BucketOwnerPreferred" }
}

resource "aws_s3_bucket_policy" "b" {
  bucket     = aws_s3_bucket.b.id
  depends_on = [aws_s3_bucket_public_access_block.b]
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = "*", Action = "s3:GetObject", Resource = "${aws_s3_bucket.b.arn}/public/*" }]
  })
}

resource "aws_s3_bucket_website_configuration" "b" {
  bucket = aws_s3_bucket.b.id
  index_document { suffix = "index.html" }
}

resource "aws_s3_object" "o" {
  bucket       = aws_s3_bucket.b.id
  key          = "public/hello.txt"
  content      = "hello"
  content_type = "text/plain"
  tags         = { kind = "greeting" }
  depends_on   = [aws_s3_bucket_versioning.b]
}
