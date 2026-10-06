create policy.yml

- !policy
  id: secrets
  body:
    - !variable weather_api_key

- !host weather-service

- !permit
  role: !host weather-service
  privileges: [read, execute]
  resource: !variable secrets/weather_api_key
