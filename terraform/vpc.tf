# 既存のVPCを参照
data "aws_vpc" "existing" {
  id = "vpc-0d61594cfc8eafef8" 
}

# 既存のサブネットを参照
data "aws_subnet" "existing_a" {
  id = "subnet-067193cfe466619f4" 
}

data "aws_subnet" "existing_c" {
  id = "subnet-046a6ff848f36a127" 
}