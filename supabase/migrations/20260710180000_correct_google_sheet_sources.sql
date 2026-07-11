-- Bind each office import source to its actual Meta Leads spreadsheet/tab.
update public.lead_import_sources src
set
  spreadsheet_id = '16Y1Qn7YyILnjVzoscCIxo9ySR0pAC4g8otKpFKe4HOM',
  sheet_name = 'KOLSS Kyiv - Wooden Furniture (2)',
  header_row = 1
from public.offices office
where src.office_id = office.id
  and office.code = 'kyiv';

update public.lead_import_sources src
set
  spreadsheet_id = '1TPy92k1QBM15dcyjSGm47xUfBnM_VLV-oQSbhPkTxcc',
  sheet_name = 'Sheet1',
  header_row = 1
from public.offices office
where src.office_id = office.id
  and office.code = 'warsaw';
