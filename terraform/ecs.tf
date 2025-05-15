resource "aws_ecs_cluster" "mirage-ecs" {
  name = var.project
  
  tags = {
    Name = var.project
  }
}

resource "aws_ecs_service" "mirage-ecs" {
  name            = var.project
  cluster         = aws_ecs_cluster.mirage-ecs.id
  task_definition = aws_ecs_task_definition.mirage-ecs.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = [data.aws_subnet.existing_a.id, data.aws_subnet.existing_c.id]
    security_groups  = [aws_security_group.default.id]
    assign_public_ip = true
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.mirage-ecs-http.arn
    container_name   = "mirage-ecs"
    container_port   = 80
  }

  depends_on = [aws_lb_listener.mirage-ecs-https]
}
resource "aws_ecs_task_definition" "mirage-ecs" {
  family                   = var.project
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = data.aws_iam_role.ecs-task-execiton.arn
  task_role_arn            = aws_iam_role.task.arn

  container_definitions = jsonencode([
    {
      name      = "mirage-ecs"
      image     = "ghcr.io/acidlemon/mirage-ecs:v2.2.1"
      essential = true
      
      portMappings = [
        {
          containerPort = 80
          hostPort      = 80
          protocol      = "tcp"
        }
      ]
      
      environment = [
        {
          name  = "MIRAGE_DOMAIN"
          value = ".mirage-ecs-test.${var.domain}"
        },
        {
          name  = "MIRAGE_CONF"
          value = "s3://${aws_s3_bucket.mirage-ecs.bucket}/config.yaml"
        }
      ]
      
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.mirage-ecs.name
          "awslogs-region"        = "ap-northeast-1"
          "awslogs-stream-prefix" = "mirage-ecs"
        }
      }
    }
  ])
}
