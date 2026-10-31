-- 由 cmd/config-tool seed 生成，不要手改。
-- 权威源：config/terminals/*.json 与 config/thresholds.json。
-- 只重建配置域投影，不触碰测量、报文、告警等业务表。

SET search_path TO monitor, public;

BEGIN;

DELETE FROM threshold;
DELETE FROM terminal;

INSERT INTO terminal (terminal_id, esn, workshop, line, enabled, config_version)
  VALUES ('ESN-ZX-0001', 'ESN-ZX-0001', '一号车间', '机加工产线', TRUE, '1.0.0');

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0001', 'A', '/dev/ttyV0', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'A', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'A', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0001', 'A', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'A', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'A', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0001', 'A', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0001', 'A', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0001', 'A', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0001', 'A', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0001', 'A', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'A', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'A', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0001', 'A', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'A', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'A', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0001', 'A', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0001', 'A', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'A', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'A', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0001', 'B', '/dev/ttyV1', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'B', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'B', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0001', 'B', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'B', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'B', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0001', 'B', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0001', 'B', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0001', 'B', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0001', 'B', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0001', 'B', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'B', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'B', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0001', 'B', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'B', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'B', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0001', 'B', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0001', 'B', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0001', 'B', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0001', 'B', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO terminal (terminal_id, esn, workshop, line, enabled, config_version)
  VALUES ('ESN-ZX-0002', 'ESN-ZX-0002', '二号车间', '装配产线', TRUE, '1.0.0');

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0002', 'A', '/dev/ttyV0', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'A', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'A', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0002', 'A', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'A', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'A', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0002', 'A', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0002', 'A', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0002', 'A', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0002', 'A', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0002', 'A', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'A', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'A', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0002', 'A', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'A', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'A', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0002', 'A', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0002', 'A', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'A', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'A', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0002', 'B', '/dev/ttyV1', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'B', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'B', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0002', 'B', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'B', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'B', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0002', 'B', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0002', 'B', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0002', 'B', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0002', 'B', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0002', 'B', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'B', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'B', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0002', 'B', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'B', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'B', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0002', 'B', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0002', 'B', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0002', 'B', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0002', 'B', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO terminal (terminal_id, esn, workshop, line, enabled, config_version)
  VALUES ('ESN-ZX-0003', 'ESN-ZX-0003', '三号车间', '焊接产线', TRUE, '1.0.0');

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0003', 'A', '/dev/ttyV0', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'A', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'A', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0003', 'A', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'A', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'A', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0003', 'A', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0003', 'A', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0003', 'A', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0003', 'A', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0003', 'A', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'A', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'A', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0003', 'A', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'A', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'A', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0003', 'A', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0003', 'A', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'A', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'A', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0003', 'B', '/dev/ttyV1', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'B', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'B', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0003', 'B', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'B', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'B', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0003', 'B', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0003', 'B', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0003', 'B', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0003', 'B', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0003', 'B', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'B', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'B', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0003', 'B', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'B', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'B', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0003', 'B', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0003', 'B', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0003', 'B', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0003', 'B', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO terminal (terminal_id, esn, workshop, line, enabled, config_version)
  VALUES ('ESN-ZX-0004', 'ESN-ZX-0004', '四号车间', '涂装产线', TRUE, '1.0.0');

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0004', 'A', '/dev/ttyV0', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'A', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'A', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0004', 'A', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'A', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'A', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0004', 'A', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0004', 'A', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0004', 'A', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0004', 'A', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0004', 'A', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'A', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'A', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0004', 'A', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'A', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'A', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0004', 'A', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0004', 'A', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'A', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'A', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0004', 'B', '/dev/ttyV1', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'B', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'B', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0004', 'B', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'B', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'B', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0004', 'B', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0004', 'B', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0004', 'B', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0004', 'B', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0004', 'B', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'B', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'B', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0004', 'B', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'B', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'B', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0004', 'B', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0004', 'B', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0004', 'B', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0004', 'B', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO terminal (terminal_id, esn, workshop, line, enabled, config_version)
  VALUES ('ESN-ZX-0005', 'ESN-ZX-0005', '五号车间', '总装产线', TRUE, '1.0.0');

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0005', 'A', '/dev/ttyV0', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'A', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'A', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0005', 'A', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'A', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'A', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0005', 'A', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0005', 'A', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0005', 'A', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0005', 'A', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0005', 'A', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'A', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'A', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0005', 'A', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'A', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'A', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0005', 'A', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0005', 'A', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'A', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'A', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, parity, timeout_ms, retry_count, inter_frame_ms)
  VALUES ('ESN-ZX-0005', 'B', '/dev/ttyV1', 9600, 8, 1, 'N', 1000, 2, 4);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'B', 1, '电机状态监测仪', 'motor_monitor');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'B', 1, 'motor_temp', '电机绕组温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 200),
  ('ESN-ZX-0005', 'B', 1, 'motor_vib', '电机振动速度', 'YC', 'uint16', 1, 1, 0.01, 0, 'mm/s', 0, 50);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'B', 2, '三相电量仪表', 'power_meter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'B', 2, 'ia', 'A 相电流', 'YC', 'uint16', 0, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0005', 'B', 2, 'ib', 'B 相电流', 'YC', 'uint16', 1, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0005', 'B', 2, 'ic', 'C 相电流', 'YC', 'uint16', 2, 1, 0.01, 0, 'A', 0, 200),
  ('ESN-ZX-0005', 'B', 2, 'uab', '线电压', 'YC', 'uint16', 3, 1, 0.1, 0, 'V', 0, 500),
  ('ESN-ZX-0005', 'B', 2, 'p_total', '有功功率', 'YC', 'uint16', 4, 1, 0.01, 0, 'kW', 0, 600),
  ('ESN-ZX-0005', 'B', 2, 'ep_forward', '正向有功电能', 'YC', 'uint32', 5, 2, 0.01, 0, 'kWh', 0, 42949672.95);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'B', 3, '温湿度变送器', 'temp_humidity_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'B', 3, 'env_temp', '车间环境温度', 'YC', 'int16', 0, 1, 0.1, 0, '℃', -20, 60),
  ('ESN-ZX-0005', 'B', 3, 'env_humi', '车间环境湿度', 'YC', 'uint16', 1, 1, 0.1, 0, '%RH', 0, 100);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'B', 4, '安全状态采集模块', 'safety_module');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'B', 4, 'door', '安全门磁', 'YX', 'uint16', 0, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0005', 'B', 4, 'estop', '急停按钮', 'YX', 'uint16', 1, 1, 1, 0, NULL, 0, 1),
  ('ESN-ZX-0005', 'B', 4, 'light_curtain', '安全光幕', 'YX', 'uint16', 2, 1, 1, 0, NULL, 0, 1);

INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)
  VALUES ('ESN-ZX-0005', 'B', 5, '气压变送器', 'pressure_transmitter');

INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)
  VALUES
  ('ESN-ZX-0005', 'B', 5, 'air_pressure', '压缩空气压力', 'YC', 'uint16', 0, 1, 0.001, 0, 'MPa', 0, 1.6);

INSERT INTO threshold (alarm_type, level, scope, terminal_id, bus_id, point_code, operator, threshold_value, debounce_count, enabled, config_version)
  VALUES
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'motor_temp', '>', 85, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0001', NULL, 'motor_temp', '>', 105, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'motor_vib', '>', 4.5, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0001', NULL, 'motor_vib', '>', 11.2, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'ia', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'ib', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'ic', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0001', NULL, 'ia', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0001', NULL, 'ib', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0001', NULL, 'ic', '>', 100, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'uab', '<', 342, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'uab', '>', 418, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'env_temp', '>', 35, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'env_humi', '>', 80, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'air_pressure', '<', 0.45, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0001', NULL, 'air_pressure', '>', 0.75, 3, TRUE, '1.0.0'),
  ('STATE_ALARM', 3, 'POINT', 'ESN-ZX-0001', NULL, 'door', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0001', NULL, 'estop', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0001', NULL, 'light_curtain', '>=', 1, 0, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'motor_temp', '>', 85, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0002', NULL, 'motor_temp', '>', 105, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'motor_vib', '>', 4.5, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0002', NULL, 'motor_vib', '>', 11.2, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'ia', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'ib', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'ic', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0002', NULL, 'ia', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0002', NULL, 'ib', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0002', NULL, 'ic', '>', 100, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'uab', '<', 342, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'uab', '>', 418, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'env_temp', '>', 35, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'env_humi', '>', 80, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'air_pressure', '<', 0.45, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0002', NULL, 'air_pressure', '>', 0.75, 3, TRUE, '1.0.0'),
  ('STATE_ALARM', 3, 'POINT', 'ESN-ZX-0002', NULL, 'door', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0002', NULL, 'estop', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0002', NULL, 'light_curtain', '>=', 1, 0, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'motor_temp', '>', 85, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0003', NULL, 'motor_temp', '>', 105, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'motor_vib', '>', 4.5, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0003', NULL, 'motor_vib', '>', 11.2, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'ia', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'ib', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'ic', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0003', NULL, 'ia', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0003', NULL, 'ib', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0003', NULL, 'ic', '>', 100, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'uab', '<', 342, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'uab', '>', 418, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'env_temp', '>', 35, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'env_humi', '>', 80, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'air_pressure', '<', 0.45, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0003', NULL, 'air_pressure', '>', 0.75, 3, TRUE, '1.0.0'),
  ('STATE_ALARM', 3, 'POINT', 'ESN-ZX-0003', NULL, 'door', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0003', NULL, 'estop', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0003', NULL, 'light_curtain', '>=', 1, 0, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'motor_temp', '>', 85, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0004', NULL, 'motor_temp', '>', 105, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'motor_vib', '>', 4.5, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0004', NULL, 'motor_vib', '>', 11.2, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'ia', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'ib', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'ic', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0004', NULL, 'ia', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0004', NULL, 'ib', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0004', NULL, 'ic', '>', 100, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'uab', '<', 342, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'uab', '>', 418, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'env_temp', '>', 35, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'env_humi', '>', 80, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'air_pressure', '<', 0.45, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0004', NULL, 'air_pressure', '>', 0.75, 3, TRUE, '1.0.0'),
  ('STATE_ALARM', 3, 'POINT', 'ESN-ZX-0004', NULL, 'door', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0004', NULL, 'estop', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0004', NULL, 'light_curtain', '>=', 1, 0, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'motor_temp', '>', 85, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0005', NULL, 'motor_temp', '>', 105, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'motor_vib', '>', 4.5, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0005', NULL, 'motor_vib', '>', 11.2, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'ia', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'ib', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'ic', '>', 80, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0005', NULL, 'ia', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0005', NULL, 'ib', '>', 100, 3, TRUE, '1.0.0'),
  ('HIGH_HIGH_LIMIT', 3, 'POINT', 'ESN-ZX-0005', NULL, 'ic', '>', 100, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'uab', '<', 342, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'uab', '>', 418, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'env_temp', '>', 35, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'env_humi', '>', 80, 3, TRUE, '1.0.0'),
  ('LOW_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'air_pressure', '<', 0.45, 3, TRUE, '1.0.0'),
  ('HIGH_LIMIT', 2, 'POINT', 'ESN-ZX-0005', NULL, 'air_pressure', '>', 0.75, 3, TRUE, '1.0.0'),
  ('STATE_ALARM', 3, 'POINT', 'ESN-ZX-0005', NULL, 'door', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0005', NULL, 'estop', '>=', 1, 0, TRUE, '1.0.0'),
  ('STATE_ALARM', 4, 'POINT', 'ESN-ZX-0005', NULL, 'light_curtain', '>=', 1, 0, TRUE, '1.0.0');

COMMIT;

-- 初始化断言 D-01~D-04
DO $$
DECLARE
  n_point     INT; n_terminal INT; n_device INT; n_threshold INT;
BEGIN
  SELECT count(*) INTO n_point     FROM point;
  SELECT count(*) INTO n_terminal  FROM terminal;
  SELECT count(*) INTO n_device    FROM device;
  SELECT count(*) INTO n_threshold FROM threshold;
  RAISE NOTICE 'D-01 point=% D-02 terminal=% D-03 device=% D-04 threshold=%',
    n_point, n_terminal, n_device, n_threshold;
END $$;
